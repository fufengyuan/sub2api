package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// cnUpstreamPlatforms 三渠道平台标识。命中时网关 Chat Completions 直调
// CnUpstreamService 的分池账号，而非走标准 OpenAI 上游。
func isCnUpstreamPlatform(platform string) bool {
	switch platform {
	case PlatformWorkBuddy, PlatformTraeWork, PlatformQoder:
		return true
	}
	return false
}

// forwardCnUpstreamChatCompletions 实现三渠道（workbuddy/traework/qoder）的
// 最小 Chat 分支：按 account.Platform 直调 CnUpstreamService.ChatStream 挑池内
// 健康账号，把私有 SSE 转换成 OpenAI 兼容格式回写客户端，并从输出中抽取 usage
// 按 tokens 计费（上游消耗积分，网关侧以 token 口径入账）。
func (s *OpenAIGatewayService) forwardCnUpstreamChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	_ = ctx // 上游调用超时/取消由 cnupstream 层 HttpClient 继承的 ctx 覆盖

	originalModel := gjson.GetBytes(body, "model").String()
	billingModel := account.GetMappedModel(originalModel)
	reqStream := gjson.GetBytes(body, "stream").Bool()

	if s.cnUpstream == nil && s.cnChatStream == nil {
		return nil, fmt.Errorf("cnupstream: service not wired")
	}

	// 上游按计费模型下发（客户端模型名经映射后才是上游认识的模型）。
	forwardBody := body
	if billingModel != "" && billingModel != originalModel {
		rewritten, err := sjson.SetBytes(body, "model", billingModel)
		if err == nil {
			forwardBody = rewritten
		}
	}

	rc, status, respBody, err := s.cnInvokeChatStream(account.Platform, forwardBody)
	if err != nil {
		// 传输层失败 / 无健康账号：未写出任何响应，交由 handler 统一补 502。
		return nil, err
	}
	if rc != nil {
		defer func() { _ = rc.Close() }()
	}
	if status >= 400 {
		// 上游 4xx/5xx：三种渠道上层已把失败账号冷却/禁用并轮换，走到这里说明
		// 全部账号不可用。直接落可读错误，handler 会补写统一失败响应。
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = fmt.Sprintf("cnupstream upstream %s http %d", account.Platform, status)
		} else if len(msg) > 512 {
			msg = msg[:512]
		}
		return nil, fmt.Errorf("cnupstream: %s upstream error (http %d): %s", account.Platform, status, msg)
	}

	if s.cnUpstream == nil {
		return nil, fmt.Errorf("cnupstream: service not wired")
	}
	upstream := s.cnUpstream.PlatformUpstream(account.Platform)
	if upstream == nil {
		return nil, fmt.Errorf("cnupstream: no upstream for %s", account.Platform)
	}

	firstTokenMs := int(time.Since(startTime).Milliseconds())
	var usage OpenAIUsage
	buildResult := func() *OpenAIForwardResult {
		return &OpenAIForwardResult{
			Usage:         usage,
			Model:         originalModel,
			BillingModel:  billingModel,
			UpstreamModel: billingModel,
			Stream:        reqStream,
			Duration:      time.Since(startTime),
			FirstTokenMs:  &firstTokenMs,
		}
	}

	if reqStream {
		cw := newCnUsageCapturingWriter(c.Writer, &usage)
		if err := upstream.Stream(cw, rc); err != nil {
			// 流已部分写出（至少状态头/首个 chunk）时照常计费，避免半途 SSE 丢计费。
			if cw.written > 0 {
				return buildResult(), nil
			}
			return nil, err
		}
		return buildResult(), nil
	}

	// 非流式：聚合完整响应，抽取 usage，写 JSON。
	agg, err := upstream.Aggregate(rc)
	if err != nil {
		if !isCnResponseCommitted(c) {
			return nil, err
		}
		agg = map[string]any{}
	}
	usage = usageFromCnUpstreamAggregate(agg)
	respJSON, err := json.Marshal(agg)
	if err != nil {
		return nil, fmt.Errorf("cnupstream: marshal aggregated response: %w", err)
	}
	c.Data(http.StatusOK, "application/json", respJSON)
	return buildResult(), nil
}

// cnInvokeChatStream 分发 ChatStream 调用：默认走 CnUpstreamService，测试可注入
// cnChatStream 覆盖（见 OpenAIGatewayService.cnChatStream 字段）。
func (s *OpenAIGatewayService) cnInvokeChatStream(platform string, body []byte) (io.ReadCloser, int, []byte, error) {
	if s.cnChatStream != nil {
		return s.cnChatStream(platform, body)
	}
	return s.cnUpstream.ChatStream(platform, body)
}

func isCnResponseCommitted(c *gin.Context) bool {
	return c != nil && c.Writer != nil && c.Writer.Written()
}

// usageFromCnUpstreamAggregate 从聚合后的 OpenAI chat.completion 对象抽取 usage。
func usageFromCnUpstreamAggregate(resp map[string]any) OpenAIUsage {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return OpenAIUsage{}
	}
	usage := OpenAIUsage{
		InputTokens:              cnUsageInt(u, "prompt_tokens"),
		OutputTokens:             cnUsageInt(u, "completion_tokens"),
		CacheReadInputTokens:     cnUsageInt(u, "prompt_cache_hit_tokens"),
		CacheCreationInputTokens: cnUsageInt(u, "prompt_cache_miss_tokens"),
	}
	return usage
}

func cnUsageInt(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		return cnUsageScalar(v)
	}
	return 0
}

func cnUsageScalar(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

// cnUsageCapturingWriter 包裹客户端 ResponseWriter：在透传流式 SSE 的同时，从
// 已写入的输出字节中抓取 usage（prompt/completion tokens）用于按 tokens 计费。
// 上行 Stream 每个 SSE 事件整行写出（一次 Write），本实现仍按换行做缓冲，稳妥
// 处理相邻事件合并或半行边界。
type cnUsageCapturingWriter struct {
	w       http.ResponseWriter
	usage   *OpenAIUsage
	written int
	buf     bytes.Buffer
}

var _ http.Flusher = (*cnUsageCapturingWriter)(nil)

func newCnUsageCapturingWriter(w http.ResponseWriter, usage *OpenAIUsage) *cnUsageCapturingWriter {
	return &cnUsageCapturingWriter{w: w, usage: usage}
}

func (cw *cnUsageCapturingWriter) Header() http.Header { return cw.w.Header() }

func (cw *cnUsageCapturingWriter) WriteHeader(code int) { cw.w.WriteHeader(code) }

func (cw *cnUsageCapturingWriter) Write(p []byte) (int, error) {
	cw.written += len(p)
	cw.scan(p)
	return cw.w.Write(p)
}

// Flush 实现 http.Flusher，透传到底层 writer 以实时 flush SSE。
func (cw *cnUsageCapturingWriter) Flush() {
	if f, ok := cw.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *cnUsageCapturingWriter) scan(p []byte) {
	cw.buf.Write(p)
	for {
		nl := bytes.IndexByte(cw.buf.Bytes(), '\n')
		if nl < 0 {
			break
		}
		line := make([]byte, nl)
		copy(line, cw.buf.Bytes()[:nl])
		cw.buf.Next(nl + 1)
		cw.parseLine(line)
	}
}

func (cw *cnUsageCapturingWriter) parseLine(line []byte) {
	trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r\n"))
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if u := usageFromCnUpstreamPayload(payload); u != nil {
		*cw.usage = *u
	}
}

// usageFromCnUpstreamPayload 从单个 SSE `data:` JSON payload 提取 usage；
// 未携带 usage 时返回 nil。兼容 workbuddy/traework 的标准 OpenAI 形与
// qoder 解嵌套后的标准形。
func usageFromCnUpstreamPayload(payload []byte) *OpenAIUsage {
	var chunk map[string]any
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil
	}
	u, ok := chunk["usage"].(map[string]any)
	if !ok {
		return nil
	}
	return &OpenAIUsage{
		InputTokens:              cnUsageInt(u, "prompt_tokens"),
		OutputTokens:             cnUsageInt(u, "completion_tokens"),
		CacheReadInputTokens:     cnUsageInt(u, "prompt_cache_hit_tokens"),
		CacheCreationInputTokens: cnUsageInt(u, "prompt_cache_miss_tokens"),
	}
}

// cnChatStreamFn 抽取 ChatStream 的签名，便于单测注入假上游（SSE fixture）。
type cnChatStreamFn func(platform string, body []byte) (io.ReadCloser, int, []byte, error)
