package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

// IsCnUpstreamPlatform 判断平台是否为国产多渠道（workbuddy/traework/qoder/qwenwork）。
// 供 handler 在旧版 GatewayService 转发前分流到支持 cn 的 OpenAIGatewayService。
func IsCnUpstreamPlatform(platform string) bool {
	switch platform {
	case PlatformWorkBuddy, PlatformTraeWork, PlatformQoder, PlatformQwenWork:
		return true
	}
	return false
}

// forwardCnUpstreamChatCompletions 实现三渠道（workbuddy/traework/qoder）的
// 最小 Chat 分支：按选中账号所属平台直调 CnUpstreamService.ChatStream，把私有
// SSE 转换成 OpenAI 兼容格式回写客户端，并从输出中抽取 usage 按 tokens 计费
// （上游消耗积分，网关侧以 token 口径入账）。
//
// 统一账号池后不再有跨平台 fallback：调度层（composite 组）已按账号级规则
// 在跨平台池内选号，失败账号由 handler 层 failover 循环排除后重选，本函数
// 只对选中账号的平台执行一次完整转发；未写出任何响应前的失败包装为
// UpstreamFailoverError 以接入 handler 的账号级重试。
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

	if s.cnUpstream == nil {
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
	res, err := s.forwardCnUpstreamSinglePlatform(c, account, forwardBody, originalModel, billingModel, reqStream, startTime, firstTokenMs)
	if err != nil && !isCnResponseCommitted(c) {
		// 未写出任何响应：包装成可 failover 错误，由 handler 排除本账号后重选。
		return nil, cnUpstreamFailoverError(account, err)
	}
	return res, err
}

// cnSoftRateSwitchBackoff 软限流（4008/4028 等）换号前的短退避：给上游一秒级
// 的恢复窗口，同时把单次请求的额外延迟控制在常数级（handler 侧限制最多退避
// 两次，见 FailoverState.maxDeferredSwitchBackoffs）。
const cnSoftRateSwitchBackoff = 2 * time.Second

// cnUpstreamFailoverError 把 CN 上游失败包装为 UpstreamFailoverError：
// 统一账号池下失败账号交由 handler failover 循环排除重选（8 次保险丝）。
//   - 整池冷却（*CnPoolExhaustedError）：按 429 + Retry-After 返回，让客户端
//     按最早恢复时间退避，而不是把它当可立即重试的 502 反复打上游。
//   - SSE 流内业务错误（4008/4028 配额超限、1005 权益不足）：属账号级问题，
//     标记账号作用域让调度器记录健康度；软限流类（4008/4028）附带换号退避延迟，
//     并映射为通用 429 + Retry-After —— 市面上 agent 客户端遇到 429 会自动重试，
//     而 502 多数 client 不重试。
//   - 其余传输层失败：按请求级处理，不对账号做健康惩罚。
func cnUpstreamFailoverError(account *Account, err error) *UpstreamFailoverError {
	scope := GatewayFailureScopeRequest
	status := http.StatusBadGateway
	var headers http.Header
	var backoff time.Duration

	var exhausted *CnPoolExhaustedError
	if errors.As(err, &exhausted) {
		status = http.StatusTooManyRequests
		scope = GatewayFailureScopeAccount
		if exhausted.RetryAfter > 0 {
			seconds := int(math.Ceil(exhausted.RetryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			headers = http.Header{"Retry-After": []string{strconv.Itoa(seconds)}}
		}
	} else if soloErr := cnSoloStreamError(err); soloErr != nil {
		scope = GatewayFailureScopeAccount
		if soloErr.Kind() == provider.ErrSoftRate {
			backoff = cnSoftRateSwitchBackoff
			status = http.StatusTooManyRequests
			headers = http.Header{"Retry-After": []string{strconv.Itoa(int(math.Ceil(cnSoftRateSwitchBackoff.Seconds())))}}
		}
	} else if code, ok := provider.ExtractBusinessCode(err.Error()); ok &&
		provider.ClassifyBusinessCode(code, err.Error()) == provider.ErrSoftRate {
		// 上游以 HTTP 4xx + JSON 业务码下发的间歇配额/频控：同样按软限流处理。
		scope = GatewayFailureScopeAccount
		backoff = cnSoftRateSwitchBackoff
		status = http.StatusTooManyRequests
		headers = http.Header{"Retry-After": []string{strconv.Itoa(int(math.Ceil(cnSoftRateSwitchBackoff.Seconds())))}}
	}

	return &UpstreamFailoverError{
		StatusCode:            status,
		Stage:                 GatewayFailureStageInference,
		Scope:                 scope,
		Reason:                GatewayFailureReason(fmt.Sprintf("cnupstream %s: %v", cnAccountPlatform(account), err)),
		ResponseBody:          []byte(err.Error()),
		ResponseHeaders:       headers,
		NextAccountRetryDelay: backoff,
	}
}

// cnSoloStreamError 从错误链里取 SSE 流内业务错误（未产出内容时由
// traework.StreamBeforeOutputError 包装，聚合路径直接返回 *SOLOStreamError）。
func cnSoloStreamError(err error) *traework.SOLOStreamError {
	var soloErr *traework.SOLOStreamError
	if errors.As(err, &soloErr) {
		return soloErr
	}
	return nil
}

func cnAccountPlatform(account *Account) string {
	if account == nil {
		return "unknown"
	}
	return account.Platform
}

// cnStreamWithErrorUpstream 暴露 SSE 流内业务错误检测能力的上游客户端。
type cnStreamWithErrorUpstream interface {
	StreamWithError(w http.ResponseWriter, r io.Reader, onErr func(*traework.SOLOStreamError) bool) error
}

// cnStream 流式转发：优先走 StreamWithError 识别 SSE 内 event:error
// （如 4008/4028 间歇配额超限 / 1005 权益不足），未产出内容时返回
// traework.StreamBeforeOutputError（携带业务码），由上层包装成 failover 错误换号
// 重试；无论是否已产出内容，业务错误都会经 onBusinessErr 上报，供上层回灌
// 账号冷却——否则被限流的账号下一轮仍会被优先选中。
// 不支持该能力的上游（workbuddy/qoder）回落普通 Stream。
func (s *OpenAIGatewayService) cnStream(
	upstream provider.Upstream,
	cw *cnUsageCapturingWriter,
	rc io.ReadCloser,
	onBusinessErr func(*traework.SOLOStreamError),
) error {
	seu, ok := upstream.(cnStreamWithErrorUpstream)
	if !ok {
		return upstream.Stream(cw, rc)
	}
	return seu.StreamWithError(cw, rc, func(se *traework.SOLOStreamError) bool {
		if onBusinessErr != nil && se != nil {
			onBusinessErr(se)
		}
		return true // 未产出内容时要求上层换号重试
	})
}

// forwardCnUpstreamSinglePlatform 对选中账号执行 ChatStream→SSE→usage→response
// 的完整转发；返回错误且尚未提交响应时，由 handler 层排除该账号重选。
// 无论成功与否都会把结果回灌上游池（target.Report），让被限流/权益耗尽的账号
// 进入冷却，下一轮选号自然绕开它。
func (s *OpenAIGatewayService) forwardCnUpstreamSinglePlatform(
	c *gin.Context,
	account *Account,
	forwardBody []byte,
	originalModel, billingModel string,
	reqStream bool,
	startTime time.Time,
	firstTokenMs int,
) (*OpenAIForwardResult, error) {
	if s.cnUpstream == nil {
		return nil, fmt.Errorf("cnupstream: service not wired")
	}
	platform := account.Platform
	target, err := s.cnInvokeChatStream(platform, account.ID, forwardBody)
	if err != nil {
		// 传输层失败 / 整池冷却：未写出任何响应，交由 handler 排除本账号重选。
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("cnupstream: %s returned nil target", platform)
	}
	if target.RC != nil {
		defer func() { _ = target.RC.Close() }()
	}
	rc := target.RC
	status := target.Status
	respBody := target.Body
	if status >= 400 {
		// 上游 4xx/5xx：渠道上层已把失败账号冷却/禁用并轮换，走到这里说明该平台
		// 全部账号不可用。落可读错误交由 handler 排除本账号重选。
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
		// 首个可见输出之前启动下游 keepalive 心跳：国产渠道路径此前完全依赖
		// 上游推送，长推理（思考数分钟无输出）期间下游一个字节都收不到，中间
		// nginx 的 proxy_read_timeout（600s）会按空闲把连接判死回 504。复用
		// standard passthrough 的 startOpenAISSEKeepalive：心跳写 SSE 注释行
		// 并提交响应头，刷新 nginx 读超时；首拍延迟一个 interval，绝大多数
		// 硬错误在此之前返回，仍走正常 failover/状态码链路。心跳字节由
		// OpenAICompactKeepaliveAdjustedWrittenSize 排除，不影响 cw.written 计数。
		//
		// 关键：cnUsageCapturingWriter 必须包裹 keepalive 启动【之前】的原始
		// writer，而非被替换后的包装器——否则 Stream 内首次访问 w.Header() 会触发
		// openAICompactKeepaliveWriter.Header() 的 suspend()，在首拍前就把心跳停掉。
		originalWriter := c.Writer
		stopKeepalive := func() {}
		if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
			stopKeepalive = startOpenAISSEKeepalive(c,
				time.Duration(s.cfg.Gateway.StreamKeepaliveInterval)*time.Second)
		}
		defer stopKeepalive()
		cw := newCnUsageCapturingWriter(originalWriter, &usage)
		// 已产出内容时 StreamWithError 返回 nil，业务错误只能从回调里拿；
		// 两种情况都要回灌冷却，否则同一个被限流的账号会被反复选中。
		var businessErr error
		streamErr := s.cnStream(upstream, cw, rc, func(se *traework.SOLOStreamError) {
			if businessErr == nil {
				businessErr = se
			}
		})
		if streamErr != nil {
			target.Report(streamErr)
			// 流已部分写出（至少状态头/首个 chunk）时照常计费，避免半途 SSE 丢计费；
			// 未写出任何响应时交由 handler 排除本账号重选。
			if cw.written > 0 {
				return buildResult(), nil
			}
			return nil, streamErr
		}
		target.Report(businessErr)
		return buildResult(), nil
	}

	// 非流式：聚合完整响应，抽取 usage，写 JSON。
	//
	// 聚合是「读完整个上游 SSE 才写响应」：长生成期间客户端零字节，中间 nginx
	// 的 proxy_read_timeout 会按空闲把连接判死回 504（与流式 pre-output 同源）。
	// 首个间隔超时后提交响应头并周期性写空白填充保活——JSON 解析器（RFC 8259）
	// 普遍容忍 body 前导空白，空格填充对各类客户端最安全；填充字节已让响应
	// 提交，聚合失败时不再走「未写出即换号」路径，与流式 keepalive 的 tradeoff
	// 一致（此时错误应透出而非无限换号）。
	//
	// 写最终 JSON 前必须先停填充 goroutine（互斥锁建立 happens-before），
	// 防止空格与 JSON 字节交错；defer 幂等兜底覆盖提前 return 的路径。
	var stopPadding func()
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		interval := time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
		stopPadding = s.startCnNonStreamJSONPadding(c, interval)
		defer stopPadding()
	}

	agg, err := upstream.Aggregate(rc)
	if err != nil {
		target.Report(err)
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
	if stopPadding != nil {
		stopPadding()
	}
	c.Data(http.StatusOK, "application/json", respJSON)
	return buildResult(), nil
}

// startCnNonStreamJSONPadding 为非流式聚合启动下游空白填充保活，返回幂等的
// 停止函数。首个 interval 超时后提交 200 + application/json 响应头并写一个
// 空格，此后每 interval 续写；请求取消即退出。stop 与写入共用互斥锁，返回
// 后不会再有字节写出，调用方可安全接管 ResponseWriter。
func (s *OpenAIGatewayService) startCnNonStreamJSONPadding(c *gin.Context, interval time.Duration) func() {
	if c == nil || c.Writer == nil || interval <= 0 {
		return func() {}
	}
	var mu sync.Mutex
	stopped := false
	pad := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return false
		}
		w := c.Writer
		if !w.Written() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		}
		if _, err := w.WriteString(" "); err != nil {
			stopped = true
			return false
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return true
	}
	done := make(chan struct{})
	stopCh := make(chan struct{})
	var reqDone <-chan struct{}
	if c.Request != nil {
		reqDone = c.Request.Context().Done()
	}
	go func() {
		defer close(done)
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-reqDone:
				return
			case <-timer.C:
			}
			if !pad() {
				return
			}
			timer.Reset(interval)
		}
	}()
	return func() {
		mu.Lock()
		if stopped {
			mu.Unlock()
			<-done
			return
		}
		stopped = true
		mu.Unlock()
		close(stopCh)
		<-done
	}
}

// cnInvokeChatStream 分发 ChatStream 调用：默认走 CnUpstreamService（以统一池
// 选中的账号 ID 为优先取号依据），测试可注入 cnChatStream 覆盖
// （见 OpenAIGatewayService.cnChatStream 字段）。
func (s *OpenAIGatewayService) cnInvokeChatStream(platform string, accountID int64, body []byte) (*CnChatTarget, error) {
	if s.cnChatStream != nil {
		return s.cnChatStream(platform, accountID, body)
	}
	return s.cnUpstream.ChatStream(platform, accountID, body)
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
type cnChatStreamFn func(platform string, accountID int64, body []byte) (*CnChatTarget, error)
