//go:build unit

package traework

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/stretchr/testify/require"
)

func TestPrepareBody_AlignsClientFields(t *testing.T) {
	in := []byte(`{"model":"DeepSeek-V4-Flash-Official","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	a := &auth.Auth{UID: "u123", DeviceID: "d456", MachineID: "m789"}
	out := PrepareBody(in, a)

	var obj map[string]any
	require.NoError(t, json.Unmarshal(out, &obj))

	// config_name/model 裸名，model_name 带 __dev
	require.Equal(t, "DeepSeek-V4-Flash-Official", obj["config_name"])
	require.Equal(t, "DeepSeek-V4-Flash-Official", obj["model"])
	require.Equal(t, "DeepSeek-V4-Flash-Official__dev", obj["model_name"])

	// 上下文/输出上限（JSON 数字是 float64）
	require.Equal(t, float64(1000000), obj["prompt_max_tokens"])
	require.Equal(t, float64(4096), obj["max_tokens"])

	// 会话/身份
	require.NotEmpty(t, obj["conversation_id"])
	require.NotEmpty(t, obj["session_id"])
	require.NotEmpty(t, obj["project_id"])
	require.Equal(t, "e04cdd", obj["workspace_id"])
	require.Equal(t, "FunctionCall", obj["mode"])
	require.Equal(t, IdeVersion, obj["ide_version"])
	require.Equal(t, AppID, obj["app_id"])
	require.Equal(t, "u123", obj["user_id"])
	require.Equal(t, "d456", obj["device_id"])
	require.Equal(t, "m789", obj["machine_id"])

	// uuid 格式校验
	for _, k := range []string{"conversation_id", "session_id", "project_id"} {
		s, _ := obj[k].(string)
		require.Len(t, s, 36, "%s 应为 36 位 UUID 字符串", k)
	}
}

func TestPrepareBody_NilAuth_NoIdentity(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[]}`)
	out := PrepareBody(in, nil)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(out, &obj))
	// nil auth 时 identity 字段应为空/缺失
	if _, ok := obj["user_id"]; ok {
		t.Errorf("nil auth 不应写 user_id")
	}
	// 但仍应有 model_name/prompt_max_tokens
	require.Equal(t, "glm-5.2__dev", obj["model_name"])
	require.Equal(t, float64(1000000), obj["prompt_max_tokens"])
}
