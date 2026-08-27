// login.go TraeWork 授权页 URL 构造与回调解析（移植自原 workbuddy-wild
// internal/login_trae，参数表与回调解析逻辑逐字对齐，含 PKCE 新流程）。
package traework

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// BuildAuthorizeURL 构造 Trae 授权页 URL（auth_type=local，含 PKCE code_challenge）。
// 授权完成后浏览器跳转 callbackURL（新版授权页 redirect=1 时走 POST），
// 携带 refreshToken / userJwt / authCodeInfo（PKCE 新流程）等查询参数。
func BuildAuthorizeURL(callbackURL, machineID, deviceID, codeChallenge string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", PluginVersion)
	v.Set("auth_type", "local")
	v.Set("client_id", ClientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", newUUID())
	v.Set("auth_callback_url", callbackURL)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", DeviceBrand)
	v.Set("x_device_type", "windows")
	v.Set("x_os_version", OSVersion)
	v.Set("x_env", "")
	v.Set("x_app_version", IdeVersion)
	v.Set("x_app_type", "stable")
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")
	v.Set("hide_saas_login", "true")
	v.Set("channel_name", "common")
	v.Set("click_id", "TRAE SOLOSetup-stable-"+PluginVersion)
	return ConsoleHost + "/authorization?" + v.Encode()
}

// CallbackInfo 回调解析结果（凭证字段，仅登录流程内部使用）。
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string // userJwt.Token 兜底路径（无 refreshToken 时）
	AuthCode     string // PKCE 新流程：authCodeInfo.AuthCode
}

// HasCredential 回调是否携带任一可用凭证。
func (i CallbackInfo) HasCredential() bool {
	return i.RefreshToken != "" || i.AccessToken != "" || i.AuthCode != ""
}

// ParseCallback 解析 TRAE 登录回调链接，提取凭证字段。
// 回调形如：
//
//	http://127.0.0.1:PORT/authorize?refreshToken=...&userInfo={...}&userJwt={...}
//	或新流程：?authCodeInfo={AuthCode,...}&host=...&userInfo={...}
//
// refreshToken 优先；缺失时回退 userJwt.RefreshToken（URL 编码 JSON），
// 再兜底 userJwt.Token 作为 accessToken，最后尝试 authCodeInfo.AuthCode（PKCE 新流程）。
func ParseCallback(rawURL string) CallbackInfo {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return CallbackInfo{}
	}
	return ParseCallbackValues(u.Query())
}

// ParseCallbackValues 从回调查询参数提取凭证字段（POST 回调的 body 字段
// 由调用方先合并进 url.Values）。
func ParseCallbackValues(q url.Values) CallbackInfo {
	info := CallbackInfo{}
	info.RefreshToken = q.Get("refreshToken")
	if info.RefreshToken != "" {
		return info
	}
	userJwt := parseJSONParam(q.Get("userJwt"))
	info.RefreshToken = getString(userJwt, "RefreshToken")
	if info.RefreshToken != "" {
		return info
	}
	info.AccessToken = getString(userJwt, "Token")
	if info.AccessToken != "" {
		return info
	}
	// PKCE 新流程：authCodeInfo 是 URL 编码 JSON {AuthCode, ExpireAt, ExpireDuration}
	if raw := q.Get("authCodeInfo"); raw != "" {
		info.AuthCode = authCodeFromInfo(raw)
	}
	return info
}

// authCodeFromInfo 从 authCodeInfo（原始 code 或 JSON 对象）提取 AuthCode。
func authCodeFromInfo(raw string) string {
	raw = strings.TrimSpace(raw)
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) == nil {
		for _, container := range []any{m, m["Result"], m["result"]} {
			mm, ok := container.(map[string]any)
			if !ok {
				continue
			}
			if s := getString(mm, "AuthCode"); s != "" {
				return s
			}
		}
		return ""
	}
	return raw // 非 JSON：视为 code 本身
}

// parseJSONParam 解回调里 URL 编码的 JSON 参数（Query 已解一层 percent-encoding，
// 这里再容错解一层）。
func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// GenMachineID 生成 64 位 hex 的 machine_id（对齐真实客户端格式，32 随机字节）。
func GenMachineID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// GenDeviceID 生成 15 位纯数字 device_id（对齐官方客户端设备 ID 格式）。
func GenDeviceID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	n := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
	return fmt.Sprintf("%015d", n%900000000000000+100000000000000)
}

// newUUID 生成 v4 格式 UUID（真实客户端 login_trace_id 形如 0f9b52e9-a63f-48e5-9bcf-888c822a5a8e）。
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
