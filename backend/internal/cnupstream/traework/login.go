// login.go TraeWork 授权页 URL 构造（移植自原 workbuddy-wild internal/login_trae，
// 参数表与原实现逐字一致，plugin_version 用真实客户端插件版本常量）。
package traework

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
)

// BuildAuthorizeURL 构造 Trae 授权页 URL（auth_type=local）。
// 授权完成后浏览器跳转 callbackURL 并携带 refreshToken / host 查询参数
// （不是授权码，无需 PKCE）；callbackURL 建议把一次性 state 放进路径。
func BuildAuthorizeURL(callbackURL, machineID, deviceID string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", PluginVersion)
	v.Set("auth_type", "local")
	v.Set("client_id", ClientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", randHex(8))
	v.Set("auth_callback_url", callbackURL)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", "PC")
	v.Set("x_device_type", "PC")
	v.Set("x_os_version", "1.0")
	v.Set("x_app_version", IdeVersion)
	v.Set("x_app_type", "stable")
	return ConsoleHost + "/authorization?" + v.Encode()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
