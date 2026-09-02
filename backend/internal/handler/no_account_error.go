package handler

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// noAccountErrorClassification describes the HTTP response to emit when
// account selection failed with ErrNoAvailableAccounts. Handlers obtain it
// via classifyNoAccountError and choose between:
//
//   - 404 model_not_found — the group has accounts, but none of them are
//     configured to serve the requested model (config / typo / unsupported
//     model). Returning 503 here misleads operators and trips reverse-proxy
//     health checks; 404 lets the client surface the real problem.
//
//   - 503 api_error — accounts that could serve the model exist but are
//     temporarily exhausted (rate limit, quota auto-pause, runtime block) OR
//     the group has no accounts at all. Both stay on 503 because retrying
//     after a backoff can plausibly succeed (or, in the empty-pool case, the
//     operator may be in the middle of adding accounts).
type noAccountErrorClassification struct {
	Status        int
	ErrType       string
	Message       string
	ModelNotFound bool // true when this is a 404 model_not_found classification
}

var selectionModelRateLimitedPattern = regexp.MustCompile(`(?:model_rate_limited|rate_limited)=(\d+)`)

// classifySelectionFailureError preserves the scheduler's compact reason when
// every model-capable account is temporarily rate limited.
func classifySelectionFailureError(err error, fallback noAccountErrorClassification) noAccountErrorClassification {
	if err == nil {
		return fallback
	}
	match := selectionModelRateLimitedPattern.FindStringSubmatch(strings.ToLower(err.Error()))
	if len(match) != 2 {
		return fallback
	}
	count, parseErr := strconv.Atoi(match[1])
	if parseErr != nil || count <= 0 {
		return fallback
	}
	return noAccountErrorClassification{
		Status:  http.StatusTooManyRequests,
		ErrType: "rate_limit_error",
		Message: "All available accounts are currently rate-limited. Please retry later.",
	}
}

// classifyNoAccountError decides between 404 model_not_found and 503
// api_error for "no available accounts" failures.
//
// The classifier intentionally does not consume the original error: the
// selection layer never tells us *why* the pool came up empty (rate-limited
// vs. unsupported model are both wrapped as ErrNoAvailableAccounts). Instead
// we re-check pool composition through DiagnoseModelAvailabilityForPlatform.
// Its dedicated database query considers only persistent eligibility
// (active status + schedulable setting) and model_mapping, bypassing scheduler
// snapshots and transient filters. That guarantees a 404 is only returned
// when persistent account/group/model configuration must change before the
// request can succeed.
//
// routingModel is the model name that account selection actually compared
// against (i.e. after group-level dispatch mapping). displayModel is the
// raw model the caller asked for; it is used only in the user-facing error
// message so that internal mapping details don't leak. Most callers pass
// the same value for both.
//
// platform is the platform the request was routed to (use
// service.PlatformOpenAI / PlatformAnthropic / PlatformGemini). It is
// required because Anthropic/Gemini routes additionally surface
// mixed-scheduled Antigravity accounts; passing the wrong platform would
// flip a legitimate 503 to a misleading 404 (or vice versa).
func classifyNoAccountError(
	ctx context.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
	platform string,
) noAccountErrorClassification {
	routingModel = strings.TrimSpace(routingModel)
	displayModel = strings.TrimSpace(displayModel)
	if displayModel == "" {
		displayModel = routingModel
	}
	if diag == nil || apiKey == nil || apiKey.GroupID == nil || routingModel == "" {
		// 缺少诊断上下文时，仍尽量给用户可操作的提示（含模型名），
		// 而不是干巴巴的 "Service temporarily unavailable"。
		return noAccountErrorClassification{
			Status:  http.StatusServiceUnavailable,
			ErrType: "api_error",
			Message: composeModelUnavailableMessage(displayModel),
		}
	}

	result := diag.DiagnoseModelAvailabilityForPlatform(ctx, apiKey.GroupID, routingModel, platform)
	if result.HasAccountsInPool && !result.HasModelSupport {
		return noAccountErrorClassification{
			Status:        http.StatusNotFound,
			ErrType:       "model_not_found",
			Message:       fmt.Sprintf("Model %q is not supported by any configured account in this group. Please switch to a supported model.", displayModel),
			ModelNotFound: true,
		}
	}
	return noAccountErrorClassification{
		Status:  http.StatusServiceUnavailable,
		ErrType: "api_error",
		Message: composeModelUnavailableMessage(displayModel),
	}
}

// composeModelUnavailableMessage 构造"模型当前不可用/请更换模型"的友好提示。
// 该模型在分组内有账号配置(池里有映射)但当前账号全被限流/冷却/不可调度
// (如 glm-5.3-flash 仅 workbuddy 支持、workbuddy 全限流)时，返回 503 并提示
// 用户稍后重试或更换模型，而不是只给生硬的 HTTP 错误。
func composeModelUnavailableMessage(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "No available accounts are currently usable. Please retry later or switch to another model."
	}
	return fmt.Sprintf("Model %q has no currently available accounts (upstream channels are rate-limited or temporarily unavailable). Please retry later or switch to another model.", model)
}

// classifyNoAccountErrorFromGin is a thin wrapper that forwards the gin
// context's underlying request context. Most call sites already have a
// *gin.Context handy, so this keeps the call sites uncluttered.
func classifyNoAccountErrorFromGin(
	c *gin.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
	platform string,
) noAccountErrorClassification {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	classification := classifyNoAccountError(ctx, diag, apiKey, routingModel, displayModel, platform)
	if classification.ModelNotFound {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
	}
	return classification
}

func classifyOpenAICompatibleNoAccountErrorFromGin(
	c *gin.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel string,
	displayModel string,
) noAccountErrorClassification {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	return classifyNoAccountErrorFromGin(
		c,
		diag,
		apiKey,
		routingModel,
		displayModel,
		openAICompatibleRequestPlatform(ctx, apiKey),
	)
}

// composeNoAccountSelectionMessage builds a single-meaning, non-duplicated
// user-facing message for a "no available accounts" selection failure.
//
// Many selection errors already begin with the standard prefix (e.g. the
// scheduler returns "no available accounts supporting model: foo"). Prefixing
// them again produced "No available accounts: no available accounts ...";
// this helper reuses the detail verbatim when it already carries the prefix.
func composeNoAccountSelectionMessage(err error) string {
	if err == nil {
		return ""
	}
	detail := strings.TrimSpace(err.Error())
	if strings.HasPrefix(strings.ToLower(detail), "no available ") ||
		strings.HasPrefix(strings.ToLower(detail), "no healthy ") {
		return detail
	}
	return "No available accounts: " + detail
}

// noAccountSelectionClientMessage 计算选号失败最终返回给客户端的 message。
// 优先给出含模型名与"更换模型"指引的友好消息(cls.Message)；对 429 软限流保留
// classifier 的"稍后重试"提示；对 503 若 scheduler 携带了有价值的诊断 detail
// (如 "supporting model: x (...)"，区别于裸 "no available accounts")则附带其上，
// 便于用户/运维定位，同时不丢失换模型指引。
func noAccountSelectionClientMessage(cls noAccountErrorClassification, err error) string {
	if cls.ModelNotFound {
		return cls.Message
	}
	if cls.Status == http.StatusTooManyRequests {
		return cls.Message
	}
	detail := ""
	if err != nil {
		detail = strings.TrimSpace(err.Error())
	}
	if detail == "" || strings.EqualFold(detail, "no available accounts") {
		return cls.Message
	}
	return cls.Message + " " + detail
}

// ensureRateLimitRetryAfter guarantees a 429 response carries a Retry-After
// header (OpenAI spec). It reuses an upstream-provided value when present,
// otherwise falls back to a 30s default so clients can back off gracefully.
func ensureRateLimitRetryAfter(c *gin.Context, upstreamHeaders http.Header) {
	copyFailoverRetryAfter(c, upstreamHeaders)
	if c == nil || c.Writer == nil {
		return
	}
	if c.Writer.Header().Get("Retry-After") == "" {
		c.Header("Retry-After", "30")
	}
}

func openAICompatibleSelectionErrorForLog(err error, platform string) error {
	if err == nil || platform != service.PlatformGrok {
		return err
	}
	message := strings.ReplaceAll(err.Error(), "OpenAI accounts", "Grok accounts")
	if message == err.Error() {
		return err
	}
	return fmt.Errorf("%s", message)
}
