package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// openAIModelListResponse 按 OpenAI /v1/models 规范解析：
// {"object":"list","data":[{"id","object","created","owned_by"}]}
// created 必须是 **整数**（Unix 秒），agent 工具会按 int 解析。
type openAIModelListResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Created     int64  `json:"created"`
		OwnedBy     string `json:"owned_by"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	} `json:"data"`
}

// TestWriteModelsList_ConformsToOpenAISpec
// 回归:/v1/models 此前对所有非 Grok 平台输出的是 **Anthropic 风格**条目
// ({id,type,display_name,created_at:"2024-01-01T00:00:00Z"}),缺 object / created /
// owned_by,且 created_at 是字符串。agent 工具(模型一键拉取)按 OpenAI 规范解析
// data[].id 时校验 object=="model"、把 created 当整数解析而失败,表现为「拉不出模型」。
func TestWriteModelsList_ConformsToOpenAISpec(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writeModelsList(c, service.PlatformTraeWork, []string{"deepseek-v4-flash", "glm-5.3-flash"})

	require.Equal(t, http.StatusOK, w.Code)
	var got openAIModelListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))

	// 顶层必须是 {"object":"list","data":[...]}
	require.Equal(t, "list", got.Object)
	require.Len(t, got.Data, 2)

	first := got.Data[0]
	require.Equal(t, "deepseek-v4-flash", first.ID)
	// 三个此前缺失的规范字段
	require.Equal(t, "model", first.Object, "data[].object 必须是 \"model\"")
	require.NotZero(t, first.Created, "data[].created 必须是非零 Unix 时间戳")
	require.Equal(t, "traework", first.OwnedBy, "data[].owned_by 必须按平台填充")
}

// TestWriteModelsList_CreatedIsIntegerNotString
// 回归:created 必须是整数。此前输出的是 Anthropic 风格的 created_at 字符串,
// 客户端把 created 当整数解析会失败。
func TestWriteModelsList_CreatedIsIntegerNotString(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writeModelsList(c, service.PlatformAnthropic, []string{"claude-fable-5"})
	raw := w.Body.Bytes()

	// 直接断言 JSON 里 created 的类型是 number 而非 string。
	var generic struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &generic))
	require.Len(t, generic.Data, 1)

	created, ok := generic.Data[0]["created"]
	require.True(t, ok, "created 字段必须存在")
	_, isFloat := created.(float64)
	require.True(t, isFloat, "created 必须是数字, got %T (%v)", created, created)
	_, isString := created.(string)
	require.False(t, isString, "created 不能是字符串(Anthropic 风格)")
}

// TestWriteModelsList_OwnedByPerPlatform
// 回归:owned_by 按平台映射,便于 agent 工具区分模型来源。
func TestWriteModelsList_OwnedByPerPlatform(t *testing.T) {
	cases := []struct {
		platform string
		want     string
	}{
		{service.PlatformOpenAI, "openai"},
		{service.PlatformAnthropic, "anthropic"},
		{service.PlatformGemini, "google"},
		{service.PlatformWorkBuddy, "workbuddy"},
		{service.PlatformTraeWork, "traework"},
		{service.PlatformQoder, "qoder"},
		{service.PlatformQwenWork, "qwenwork"},
		{service.PlatformComposite, "sub2api"},
		{"", "sub2api"},
	}
	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			writeModelsList(c, tc.platform, []string{"m1"})

			var got openAIModelListResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			require.Len(t, got.Data, 1)
			require.Equal(t, tc.want, got.Data[0].OwnedBy)
		})
	}
}

// TestWriteModelsList_KeepsAnthropicFieldsForCompat
// 回归:/v1/models 与根 /models(Anthropic 客户端)共用 handler,部分 Anthropic
// 客户端仍读 created_at / display_name,必须保留以避免回归。
func TestWriteModelsList_KeepsAnthropicFieldsForCompat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writeModelsList(c, service.PlatformAnthropic, []string{"claude-fable-5"})

	var got openAIModelListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Data, 1)
	require.NotEmpty(t, got.Data[0].CreatedAt, "created_at 需保留以兼容 Anthropic 客户端")
	require.Equal(t, "claude-fable-5", got.Data[0].DisplayName)
}

// TestWriteModelsList_GrokStillUsesGrokShape
// 回归:Grok 走独立分支(带 reasoning 能力字段),不能被本次改动波及。
func TestWriteModelsList_GrokStillUsesGrokShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writeModelsList(c, service.PlatformGrok, []string{"grok-4.6"})

	var got openAIModelListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Data, 1)
	require.Equal(t, "grok-4.6", got.Data[0].ID)
	require.Equal(t, "model", got.Data[0].Object)
	require.Equal(t, "xai", got.Data[0].OwnedBy)
}

// TestGeminiAndClaudeFallbackModelIDsAreConverted
// 回归:Gemini / 其余平台的兜底列表(geminicli.DefaultModels / claude.DefaultModels)
// 原本直接输出 Anthropic 风格条目,现在必须经规范化,模型 ID 不丢。
func TestGeminiAndClaudeFallbackModelIDsAreConverted(t *testing.T) {
	t.Run("gemini 兜底列表含 gemini-2.5-pro", func(t *testing.T) {
		ids := geminicliModelIDs()
		require.NotEmpty(t, ids)
		require.Contains(t, ids, "gemini-2.5-pro")
	})

	t.Run("claude 兜底列表非空且经规范化输出", func(t *testing.T) {
		ids := claudeModelIDs()
		require.NotEmpty(t, ids)

		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		writeModelsList(c, service.PlatformQwenWork, ids)

		var got openAIModelListResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Len(t, got.Data, len(ids))
		for _, item := range got.Data {
			require.Equal(t, "model", item.Object)
			require.NotZero(t, item.Created)
			require.NotEmpty(t, item.OwnedBy)
		}
	})
}
