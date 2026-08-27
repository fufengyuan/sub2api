package traework

import (
	"net/url"
	"testing"
)

// 回调凭证解析优先级对齐原版 login_trae.ParseCallback：
// refreshToken > userJwt.RefreshToken > userJwt.Token > authCodeInfo.AuthCode。
func TestParseCallbackValues(t *testing.T) {
	cases := []struct {
		name string
		q    url.Values
		want CallbackInfo
	}{
		{
			name: "refresh_token_priority",
			q:    url.Values{"refreshToken": {"rt-1"}, "userJwt": {`{"Token":"t","RefreshToken":"rt-2"}`}, "authCodeInfo": {"ac"}},
			want: CallbackInfo{RefreshToken: "rt-1"},
		},
		{
			name: "userjwt_refresh_token_fallback",
			q:    url.Values{"userJwt": {`{"Token":"t","RefreshToken":"rt-2"}`}},
			want: CallbackInfo{RefreshToken: "rt-2"},
		},
		{
			name: "userjwt_token_access_fallback",
			q:    url.Values{"userJwt": {`{"Token":"t"}`}},
			want: CallbackInfo{AccessToken: "t"},
		},
		{
			name: "authcode_info_json",
			q:    url.Values{"authCodeInfo": {`{"AuthCode":"ac-1","ExpireAt":1}`}},
			want: CallbackInfo{AuthCode: "ac-1"},
		},
		{
			name: "authcode_info_raw_code",
			q:    url.Values{"authCodeInfo": {"raw-code"}},
			want: CallbackInfo{AuthCode: "raw-code"},
		},
		{
			name: "empty",
			q:    url.Values{},
			want: CallbackInfo{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseCallbackValues(c.q)
			if got != c.want {
				t.Fatalf("ParseCallbackValues = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseCallbackRawURL(t *testing.T) {
	raw := "http://127.0.0.1:8080/authorize?refreshToken=rt&host=https%3A%2F%2Fapi.trae.cn"
	info := ParseCallback(raw)
	if info.RefreshToken != "rt" {
		t.Fatalf("RefreshToken = %q, want rt", info.RefreshToken)
	}
	// URL 编码的 userJwt JSON 也能解出 RefreshToken。
	encoded := url.Values{}
	encoded.Set("userJwt", `{"RefreshToken":"rt-jwt"}`)
	info = ParseCallback("http://127.0.0.1:8080/authorize?" + encoded.Encode())
	if info.RefreshToken != "rt-jwt" {
		t.Fatalf("RefreshToken = %q, want rt-jwt", info.RefreshToken)
	}
}

func TestGenMachineIDAndDeviceID(t *testing.T) {
	m := GenMachineID()
	if len(m) != 64 {
		t.Fatalf("machine id len = %d, want 64 hex chars", len(m))
	}
	d := GenDeviceID()
	if len(d) != 15 {
		t.Fatalf("device id len = %d, want 15 digits", len(d))
	}
	for _, ch := range d {
		if ch < '0' || ch > '9' {
			t.Fatalf("device id %q contains non-digit", d)
		}
	}
}
