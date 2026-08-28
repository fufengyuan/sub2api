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

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
)

// cnUpstreamPlatforms 三渠道平台标识。命中时网关 Chat Completions 直调
// CnUpstreamService 的分池账号，而非走标准 OpenAI 上游。
func isCnUpstreamPlatform(platform string) bool {
	return IsCnUpstreamPlatform(platform)
}

// IsCnUpstreamPlatform 判断平台是否为国产多渠道（workbuddy/traework/qoder）。
// 供 handler 在旧版 GatewayService 转发前分流到支持 cn 的 OpenAIGatewayService。
func IsCnUpstreamPlatform(platform string) bool {
	switch platform {
	case PlatformWorkBuddy, PlatformTraeWork, PlatformQoder:
		return true
	}
	return false
}

// forwardCnUpstreamChatCompletions 实现三渠道（workbuddy/traework/qoder）的
// 最小 Chat 分支：按 composite 候选平台依次直调 CnUpstreamService.ChatStream 挑
// 池内健康账号，把私有 SSE 转换成 OpenAI 兼容格式回写客户端，并从输出中抽取 usage
// 按 tokens 计费（上游消耗积分，网关侧以 token 口径入账）。
//
// 跨平台 fallback：从 ctx 读取 composite 候选平台列表（account.Platform 优先），
// 前一平台在未写出任何响应前失败则尝试下一个候选平台，切换后同一请求即命中票足的健康账号。
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

	firstTokenMs := int(time.Since(startTime).Milliseconds())
	var lastErr error
	for _, platform := range s.cnFallbackPlatformOrder(ctx, account.Platform) {
		res, err := s.forwardCnUpstreamSinglePlatform(c, platform, forwardBody, originalModel, billingModel, reqStream, startTime, firstTokenMs)
		if err == nil {
			return res, nil
		}
		lastErr = err
		// 已写出响应（至少状态头/首个 chunk）时不可再跨平台 fallback，避免半途切换。
		if isCnResponseCommitted(c) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("cnupstream: all candidate platforms unavailable")
	}
	return nil, lastErr
}

// cnFallbackPlatformOrder 构造跨平台 fallback 尝试顺序：account.Platform 优先，
// 其后按 composite 候选平台顺序追加以漏的国产渠道平台（去重）。
func (s *OpenAIGatewayService) cnFallbackPlatformOrder(ctx context.Context, primary string) []string {
	order := []string{primary}
	seen := map[string]bool{primary: true}
	for _, cand := range CompositeCandidatesFromContext(ctx) {
		p := cand.Platform
		if p == "" || seen[p] || !isCnUpstreamPlatform(p) {
			continue
		}
		seen[p] = true
		order = append(order, p)
	}
	return order
}

// cnStreamWithErrorUpstream 暴露 SSE 流内业务错误检测能力的上游客户端。
type cnStreamWithErrorUpstream interface {
	StreamWithError(w http.ResponseWriter, r io.Reader, onErr func(*traework.SOLOStreamError) bool) error
}

// cnStream 流式转发：优先走 StreamWithError 识别 SSE 内 event:error
//（如 4008 间歇配额超限 / 1005 权益不足），未产出内容时返回
// traework.ErrStreamErrorBeforeOutput，让上层在未写出响应时触发跨平台 fallback；
// 不支持该能力的上游（workbuddy/qoder）回落普通 Stream。
func (s *OpenAIGatewayService) cnStream(upstream provider.Upstream, cw *cnUsageCapturingWriter, rc io.ReadCloser) error {
	seu, ok := upstream.(cnStreamWithErrorUpstream)
	if !ok {
		return upstream.Stream(cw, rc)
	}
	return seu.StreamWithError(cw, rc, func(_ *traework.SOLOStreamError) bool {
		return true // 未产出内容时要求上层 fallback
	})
}

// forwardCnUpstreamSinglePlatform 对单个平台执行 ChatStream→SSE→usage→response 的
// 完整转发；返回错误且尚未提交响应时，由上层切换到下一个候选平台。
func (s *OpenAIGatewayService) forwardCnUpstreamSinglePlatform(
	c *gin.Context,
	platform string,
	forwardBody []byte,
	originalModel, billingModel string,
	reqStream bool,
	startTime time.Time,
	firstTokenMs int,
) (*OpenAIForwardResult, error) {
	if s.cnUpstream == nil {
		return nil, fmt.Errorf("cnupstream: service not wired")
	}
	rc, status, respBody, err := s.cnInvokeChatStream(platform, forwardBody)
	if err != nil {
		// 传输层失败 / 无健康账号：未写出任何响应，交由上层尝试下一个候选平台。
		return nil, err
	}
	if rc != nil {
		defer func() { _ = rc.Close() }()
	}
	if status >= 400 {
		// 上游 4xx/5xx：渠道上层已把失败账号冷却/禁用并轮换，走到这里说明该平台
		// 全部账号不可用。落可读错误交由上层尝试下一个候选平台。
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = fmt.Sprintf("cnupstream upstream %s http %d", platform, status)
		} else if len(msg) > 512 {
			msg = msg[:512]
		}
		return nil, fmt.Errorf("cnupstream: %s upstream error (http %d): %s", platform, status, msg)
	}

	upstream := s.cnUpstream.PlatformUpstream(platform)
	if upstream == nil {
		return nil, fmt.Errorf("cnupstream: no upstream for %s", platform)
	}

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
		streamErr := s.cnStream(upstream, cw, rc)
		if streamErr != nil {
			// 流已部分写出（至少状态头/首个 chunk）时照常计费，避免半途 SSE 丢计费；
			// 未写出任何响应且是「未产出内容首遇业务错误」时，交由上层 fallback。
			if cw.written > 0 {
				return buildResult(), nil
			}
			return nil, streamErr
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
