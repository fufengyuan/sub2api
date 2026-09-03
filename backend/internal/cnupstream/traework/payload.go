package traework

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
)

// promptMaxTokens 是单次请求的上下文上限（prompt_max_tokens，对应上游识别
// 的上下文窗口）。经真实上游实测（2026-09-03）：上游接受 1000000（1M），
// 设置为 1M 即让 traework 账号直接用满 1M 上下文；"渐进探上限"时可外部改值。
var promptMaxTokens = 1000000

// PrepareBody 把 OpenAI chat/completions 请求改写为 Trae SOLO llm_utils_chat 请求。
// a 提供账号级身份字段（uid/device_id/machine_id 等），与真实客户端对齐：
// 补 model_name(__dev)/prompt_max_tokens/max_tokens/conversation_id/session_id/
// project_id/user_id/device_id/machine_id/workspace_id/mode/ide_version/app_id。
func PrepareBody(src []byte, a *auth.Auth) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = Function
	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)
			// SOLO 上游只认 system/assistant/user/tool，不认 developer。
			if role == "developer" {
				m["role"] = "system"
				role = "system"
			}
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]any); ok {
					kept := make([]any, 0, len(tcs))
					for _, tci := range tcs {
						tc, ok := tci.(map[string]any)
						if !ok {
							continue
						}
						if fn, ok := tc["function"].(map[string]any); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
						if fc, ok := tc["function_call"].(map[string]any); ok {
							name, _ := fc["name"].(string)
							if strings.TrimSpace(name) == "" {
								continue
							}
						}
						kept = append(kept, tc)
					}
					if len(kept) == 0 {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = kept
					}
				}
			}
			if !present || content == nil {
				continue
			}
			if s, ok := content.(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": s}}
			}
		}
	}
	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultConfigName
	}
	obj["config_name"] = model
	obj["model"] = model
	// model_name 对齐真实客户端：`<名>__dev`（config_name/model 保持裸名）。
	// 之前给 config_name/model 追加 __max 触发 4001，但 model_name 是独立字段，
	// 带 __dev 才是真实客户端形态（TraeWorkAssistant payload.rs）。
	obj["model_name"] = model + "__dev"
	// 上下文与输出上限对齐客户端（渐进探上限时可调 promptMaxTokens）。
	obj["prompt_max_tokens"] = promptMaxTokens
	obj["max_tokens"] = 4096
	// 会话/身份字段对齐客户端（uuid 用 stdlib crypto/rand 生成）。
	obj["conversation_id"] = genUUIDLike()
	obj["project_id"] = genUUIDLike()
	obj["session_id"] = genUUIDLike()
	obj["workspace_id"] = "e04cdd"
	obj["mode"] = "FunctionCall"
	obj["ide_version"] = IdeVersion
	obj["ide_version_code"] = IdeVersionCode
	obj["app_id"] = AppID
	if a != nil {
		if a.UID != "" {
			obj["user_id"] = a.UID
		}
		if a.DeviceID != "" {
			obj["device_id"] = a.DeviceID
		}
		if a.MachineID != "" {
			obj["machine_id"] = a.MachineID
		}
	}
	normalizeToolChoice(obj)
	normalizeTools(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// genUUIDLike 生成一个类似 UUID 的 32 位十六进制串（用于 conversation_id/
// project_id/session_id）。真实客户端用时间+常数播种；sub2api 直接用 crypto/rand
// 更安全且无需关心重复。
func genUUIDLike() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return formatUUID(b)
	}
	// crypto/rand 熵源失败（几乎不会发生）时退化为纳秒时间戳填充 16 字节，
	// 维持 UUID 格式。
	v := uint64(time.Now().UnixNano())
	for i := range b {
		b[i] = byte(v >> (8 * (i % 8)))
	}
	return formatUUID(b)
}

func formatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func normalizeToolChoice(obj map[string]any) {
	suppress := func() { delete(obj, "tools"); delete(obj, "functions") }
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
