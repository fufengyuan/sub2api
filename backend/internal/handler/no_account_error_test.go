//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type fakeDiagnoser struct {
	calls []fakeDiagnoseCall
	resp  service.ModelAvailabilityDiagnosis
}

type fakeDiagnoseCall struct {
	GroupID  *int64
	Model    string
	Platform string
}

func (f *fakeDiagnoser) DiagnoseModelAvailabilityForPlatform(
	_ context.Context,
	groupID *int64,
	model, platform string,
) service.ModelAvailabilityDiagnosis {
	f.calls = append(f.calls, fakeDiagnoseCall{
		GroupID:  groupID,
		Model:    model,
		Platform: platform,
	})
	return f.resp
}

func ptrInt64(v int64) *int64 { return &v }

// newTestGinContextWithRequest wraps the bare newTestGinContext helper
// (defined in openai_gateway_cyber_test.go) by additionally attaching a stub
// *http.Request so the classifier can extract c.Request.Context().
func newTestGinContextWithRequest() *gin.Context {
	c := newTestGinContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	return c
}

func TestClassifyNoAccountError_NilDiagnoser_Falls503(t *testing.T) {
	c := newTestGinContextWithRequest()
	apiKey := &service.APIKey{GroupID: ptrInt64(7)}

	cls := classifyNoAccountErrorFromGin(c, nil, apiKey, "gpt-5", "gpt-5", service.PlatformOpenAI)

	require.Equal(t, http.StatusServiceUnavailable, cls.Status)
	require.Equal(t, "api_error", cls.ErrType)
	require.False(t, cls.ModelNotFound)
}

func TestClassifySelectionFailureError_RateLimitedPool(t *testing.T) {
	fallback := noAccountErrorClassification{Status: http.StatusServiceUnavailable, ErrType: "api_error", Message: "Service temporarily unavailable"}

	got := classifySelectionFailureError(
		fmt.Errorf("no available accounts supporting model: gpt-5.6-sol (total=3 eligible=0 model_rate_limited=3)"),
		fallback,
	)

	require.Equal(t, http.StatusTooManyRequests, got.Status)
	require.Equal(t, "rate_limit_error", got.ErrType)
	require.Contains(t, got.Message, "rate-limited")
	require.Equal(t, fallback, classifySelectionFailureError(fmt.Errorf("model_rate_limited=0"), fallback))
	require.Equal(t, fallback, classifySelectionFailureError(fmt.Errorf("no available accounts"), fallback))
}

func TestClassifyNoAccountError_NilAPIKey_Falls503(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false}}

	cls := classifyNoAccountErrorFromGin(c, fd, nil, "gpt-5", "gpt-5", service.PlatformOpenAI)

	require.Equal(t, http.StatusServiceUnavailable, cls.Status)
	require.False(t, cls.ModelNotFound)
	require.Empty(t, fd.calls, "diagnoser must not be consulted when apiKey missing")
}

func TestClassifyNoAccountError_NilGroupID_Falls503(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false}}
	apiKey := &service.APIKey{GroupID: nil}

	cls := classifyNoAccountErrorFromGin(c, fd, apiKey, "gpt-5", "gpt-5", service.PlatformOpenAI)

	require.Equal(t, http.StatusServiceUnavailable, cls.Status)
	require.False(t, cls.ModelNotFound)
	require.Empty(t, fd.calls, "diagnoser must not be consulted when group not bound")
}

func TestClassifyNoAccountError_EmptyModel_Falls503(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false}}
	apiKey := &service.APIKey{GroupID: ptrInt64(7)}

	cls := classifyNoAccountErrorFromGin(c, fd, apiKey, "   ", "", service.PlatformOpenAI)

	require.Equal(t, http.StatusServiceUnavailable, cls.Status)
	require.False(t, cls.ModelNotFound)
	require.Empty(t, fd.calls)
}

func TestClassifyNoAccountError_ModelNotSupported_Returns404(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false}}
	apiKey := &service.APIKey{GroupID: ptrInt64(42)}

	cls := classifyNoAccountErrorFromGin(c, fd, apiKey, "gpt-5.1-codex-mini", "gpt-5.1-codex-mini", service.PlatformOpenAI)

	require.Equal(t, http.StatusNotFound, cls.Status)
	require.Equal(t, "model_not_found", cls.ErrType)
	require.True(t, cls.ModelNotFound)
	require.Contains(t, cls.Message, "gpt-5.1-codex-mini", "message must surface the requested model")

	require.Len(t, fd.calls, 1)
	require.Equal(t, "gpt-5.1-codex-mini", fd.calls[0].Model)
	require.Equal(t, service.PlatformOpenAI, fd.calls[0].Platform)
	require.NotNil(t, fd.calls[0].GroupID)
	require.Equal(t, int64(42), *fd.calls[0].GroupID)
	require.True(t, service.HasOpsClientBusinessLimited(c))
	require.Equal(t, service.OpsClientBusinessLimitedReasonLocalModelConfiguration, service.OpsClientBusinessLimitedReason(c))
}

func TestComposeNoAccountSelectionMessage_NoDuplicatePrefix(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error yields empty message",
			err:  nil,
			want: "",
		},
		{
			name: "detail already starts with standard prefix is reused verbatim",
			err:  fmt.Errorf("no available accounts"),
			want: "no available accounts",
		},
		{
			name: "detail already starts with model-specific prefix is reused verbatim",
			err:  fmt.Errorf("no available accounts supporting model: gpt-5.6-sol"),
			want: "no available accounts supporting model: gpt-5.6-sol",
		},
		{
			name: "unrelated detail gets the prefix once",
			err:  fmt.Errorf("upstream refused connection"),
			want: "No available accounts: upstream refused connection",
		},
		{
			name: "no healthy prefix is also preserved",
			err:  fmt.Errorf("no healthy account"),
			want: "no healthy account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, composeNoAccountSelectionMessage(tt.err))
		})
	}
}

func TestComposeNoAccountSelectionMessage_CI(t *testing.T) {
	// "No available accounts no available accounts" double-colon bug must never
	// resurface for the common case where the selection error already carries
	// the standard prefix.
	msg := composeNoAccountSelectionMessage(fmt.Errorf("NO AVAILABLE ACCOUNTS supporting model: made-up-model"))
	require.Equal(t, "NO AVAILABLE ACCOUNTS supporting model: made-up-model", msg)
	require.NotContains(t, msg, "No available accounts: no available accounts")
}

func TestClassifyOpenAICompatibleNoAccountError_GrokUsesGrokPlatform(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false}}
	groupID := int64(43)
	apiKey := &service.APIKey{
		GroupID: &groupID,
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformGrok,
		},
	}

	cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, fd, apiKey, "grok-4.5", "grok-4.5")

	require.Equal(t, http.StatusNotFound, cls.Status)
	require.Equal(t, "model_not_found", cls.ErrType)
	require.True(t, cls.ModelNotFound)
	require.Len(t, fd.calls, 1)
	require.Equal(t, service.PlatformGrok, fd.calls[0].Platform)
	require.True(t, service.HasOpsClientBusinessLimited(c))
	require.Equal(t, service.OpsClientBusinessLimitedReasonLocalModelConfiguration, service.OpsClientBusinessLimitedReason(c))

	logErr := openAICompatibleSelectionErrorForLog(
		fmt.Errorf("no available OpenAI accounts supporting model: grok-4.5"),
		service.PlatformGrok,
	)
	require.EqualError(t, logErr, "no available Grok accounts supporting model: grok-4.5")
}

func TestClassifyNoAccountError_PureClassifierDoesNotMarkGinContext(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false}}
	apiKey := &service.APIKey{GroupID: ptrInt64(7)}

	cls := classifyNoAccountError(c.Request.Context(), fd, apiKey, "gpt-5", "gpt-5", service.PlatformOpenAI)

	require.True(t, cls.ModelNotFound)
	require.False(t, service.HasOpsClientBusinessLimited(c))
	require.Empty(t, service.OpsClientBusinessLimitedReason(c))
}

func TestClassifyNoAccountError_HasModelSupport_KeepsRoutingMessageGenerationToCaller(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}}
	apiKey := &service.APIKey{GroupID: ptrInt64(7)}

	cls := classifyNoAccountErrorFromGin(c, fd, apiKey, "gpt-5", "gpt-5", service.PlatformOpenAI)

	require.Equal(t, http.StatusServiceUnavailable, cls.Status, "model exists somewhere — caller stays on 503")
	require.Equal(t, "api_error", cls.ErrType)
	require.False(t, cls.ModelNotFound)
}

func TestClassifyNoAccountError_ModelSupportedOnlyByRateLimitedAccount_Returns503(t *testing.T) {
	c := newTestGinContextWithRequest()
	// The diagnoser's configured-state lookup still sees the model-supporting
	// account even though normal scheduling has excluded it during cooldown.
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}}
	apiKey := &service.APIKey{GroupID: ptrInt64(7)}

	cls := classifyNoAccountErrorFromGin(c, fd, apiKey, "claude-opus-4-8", "claude-opus-4-8", service.PlatformAnthropic)

	require.Equal(t, http.StatusServiceUnavailable, cls.Status)
	require.Equal(t, "api_error", cls.ErrType)
	require.False(t, cls.ModelNotFound, "temporary account cooldown must remain retryable")
}

func TestClassifyNoAccountError_NoAccountsInPool_Stays503(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: false, HasModelSupport: false}}
	apiKey := &service.APIKey{GroupID: ptrInt64(7)}

	cls := classifyNoAccountErrorFromGin(c, fd, apiKey, "gpt-5", "gpt-5", service.PlatformOpenAI)

	require.Equal(t, http.StatusServiceUnavailable, cls.Status, "empty pool is a service-availability issue, not a model issue")
	require.False(t, cls.ModelNotFound)
}

func TestClassifyNoAccountError_DisplayModelOverridesRoutingForMessage(t *testing.T) {
	c := newTestGinContextWithRequest()
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false}}
	apiKey := &service.APIKey{GroupID: ptrInt64(7)}

	cls := classifyNoAccountErrorFromGin(c, fd, apiKey, "gpt-5", "claude-3-fancy", service.PlatformOpenAI)

	require.True(t, cls.ModelNotFound)
	require.Contains(t, cls.Message, "claude-3-fancy", "user-facing message must reference the model the user asked for, not the post-mapping routing model")
	require.Len(t, fd.calls, 1)
	require.Equal(t, "gpt-5", fd.calls[0].Model, "diagnosis must run against the routing model (post group dispatch mapping)")
}

func TestClassifyNoAccountError_FromGin_NilContextStillSafe(t *testing.T) {
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false}}
	apiKey := &service.APIKey{GroupID: ptrInt64(7)}

	cls := classifyNoAccountErrorFromGin(nil, fd, apiKey, "gpt-5", "gpt-5", service.PlatformOpenAI)

	require.Equal(t, http.StatusNotFound, cls.Status, "even with a nil gin context the classifier must still run and yield a coherent response")
	require.True(t, cls.ModelNotFound)
	require.False(t, service.HasOpsClientBusinessLimited(nil))
	require.Empty(t, service.OpsClientBusinessLimitedReason(nil))
}

func TestComposeModelUnavailableMessage(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{
			name:  "empty model falls back to generic retry hint",
			model: "",
			want:  "No available accounts are currently usable. Please retry later or switch to another model.",
		},
		{
			name:  "model name included with switch hint",
			model: "glm-5.3-flash",
			want:  `Model "glm-5.3-flash" has no currently available accounts (upstream channels are rate-limited or temporarily unavailable). Please retry later or switch to another model.`,
		},
		{
			name:  "whitespace model trimmed",
			model: "  gpt-5  ",
			want:  `Model "gpt-5" has no currently available accounts (upstream channels are rate-limited or temporarily unavailable). Please retry later or switch to another model.`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, composeModelUnavailableMessage(tt.model))
		})
	}
}

func TestNoAccountSelectionClientMessage(t *testing.T) {
	modelNotFound := noAccountErrorClassification{Status: http.StatusNotFound, ErrType: "model_not_found", Message: `Model "x" not supported here`, ModelNotFound: true}
	rateLimited := noAccountErrorClassification{Status: http.StatusTooManyRequests, ErrType: "rate_limit_error", Message: "All available accounts are currently rate-limited. Please retry later."}
	svcUnavail := noAccountErrorClassification{Status: http.StatusServiceUnavailable, ErrType: "api_error", Message: composeModelUnavailableMessage("glm-5.3-flash")}

	t.Run("model_not_found uses classifier message verbatim", func(t *testing.T) {
		require.Equal(t, modelNotFound.Message, noAccountSelectionClientMessage(modelNotFound, fmt.Errorf("whatever")))
	})
	t.Run("429 keeps classifier retry hint", func(t *testing.T) {
		require.Equal(t, rateLimited.Message, noAccountSelectionClientMessage(rateLimited, fmt.Errorf("no available accounts")))
	})
	t.Run("503 with bare no-available uses friendly message", func(t *testing.T) {
		got := noAccountSelectionClientMessage(svcUnavail, fmt.Errorf("no available accounts"))
		require.Equal(t, svcUnavail.Message, got)
		require.Contains(t, got, "glm-5.3-flash")
		require.Contains(t, got, "switch to another model")
	})
	t.Run("503 with diagnostic detail appends it", func(t *testing.T) {
		err := fmt.Errorf("no available accounts supporting model: glm-5.3-flash (total=2 eligible=0 model_rate_limited=2)")
		got := noAccountSelectionClientMessage(svcUnavail, err)
		require.Contains(t, got, "glm-5.3-flash")
		require.Contains(t, got, "switch to another model")
		require.Contains(t, got, "model_rate_limited=2")
	})
	t.Run("503 with nil error uses friendly message", func(t *testing.T) {
		require.Equal(t, svcUnavail.Message, noAccountSelectionClientMessage(svcUnavail, nil))
	})
}

func TestChatCompletionsErrorResponse_OpenAICompatibleShape(t *testing.T) {
	// OpenAI Chat Completions error 对象标准字段为 message/type/param/code。
	// 无具体 param/code 时应输出 null（而非缺失字段），保证下游按标准结构解析。
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	(&GatewayHandler{}).chatCompletionsErrorResponse(c, http.StatusServiceUnavailable, "api_error", "no available accounts")

	require.Equal(t, http.StatusServiceUnavailable, c.Writer.Status())
	require.JSONEq(t, `{"error":{"message":"no available accounts","type":"api_error","param":null,"code":null}}`, w.Body.String())
}

func TestResponsesErrorResponse_OpenAIShape(t *testing.T) {
	// OpenAI Responses API error 对象标准字段为 code/message/param/type。
	// 补全 param/type，避免字段缺失导致下游解析异常。
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	(&GatewayHandler{}).responsesErrorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "rate limited")

	require.Equal(t, http.StatusTooManyRequests, c.Writer.Status())
	require.JSONEq(t, `{"error":{"code":"rate_limit_error","message":"rate limited","param":null,"type":"rate_limit_error"}}`, w.Body.String())
}
