package provider

import "testing"

func TestClassifyBusinessCode(t *testing.T) {
	cases := []struct {
		name string
		code int64
		msg  string
		want ErrKind
	}{
		{"4008 配额超限按软限流", 4008, "Your requests have exceeded the quota.", ErrSoftRate},
		{"4028 频控按软限流", 4028, "your requests are too frequent", ErrSoftRate},
		{"9074 操作过频按软限流", 9074, "operation too frequent", ErrSoftRate},
		{"1005 权益不足按硬冷却", 1005, "plan expired", ErrHardCredit},
		// 请求级错误：换号必然复现，不换号也不冷却账号，直接把失败交给客户端。
		{"1001 模型不可用按请求级", 1001, "model not available", ErrRequest},
		{"4001 参数非法按请求级", 4001, "invalid model name", ErrRequest},
		{"4026 上下文超限按请求级", 4026, "We're sorry, your context length has exceeded the maximum limit.", ErrRequest},
		{"4026 中文上下文超限按请求级", 4026, "上下文长度超限", ErrRequest},
		{"未知码命中限流文案按软限流", 7000, "Rate limit exceeded, 请稍后再试", ErrSoftRate},
		{"未知码命中上下文文案按请求级", 7002, "your context length is too long", ErrRequest},
		{"未知码命中参数文案按请求级", 7003, "the param is invalid", ErrRequest},
		// 软限流文案判定必须在请求级之后，否则 "exceeded" 会被误判成可换号的间歇限流。
		{"上下文超限不被误判为软限流", 4026, "context length exceeded the quota", ErrRequest},
		{"未知码无文案兜底按客户端错误", 7001, "", ErrClient},
		// 回归：宽泛词不得把账号级/间歇性错误误判成请求级——否则本可换号自愈的
		// 限流会被直接抛给客户端，反而降低成功率。
		{"上下文已失效不按请求级", 7004, "user context is invalid", ErrClient},
		{"配额超限仍按软限流", 7005, "you have exceeded the maximum quota", ErrSoftRate},
		{"频控仍按软限流", 7006, "your requests are too frequent", ErrSoftRate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyBusinessCode(tc.code, tc.msg); got != tc.want {
				t.Fatalf("ClassifyBusinessCode(%d,%q) = %v, want %v", tc.code, tc.msg, got, tc.want)
			}
		})
	}
}

func TestExtractBusinessCode(t *testing.T) {
	cases := []struct {
		body   string
		want   int64
		wantOK bool
	}{
		{`{"code":4028,"message":"x"}`, 4028, true},
		{`{"code":"4028"}`, 4028, true},
		{`{"Code": 4008 }`, 4008, true},
		{`{"error_code":1001}`, 0, false},
		{`no code here`, 0, false},
	}
	for _, tc := range cases {
		got, ok := ExtractBusinessCode(tc.body)
		if got != tc.want || ok != tc.wantOK {
			t.Fatalf("ExtractBusinessCode(%q) = (%d,%v), want (%d,%v)", tc.body, got, ok, tc.want, tc.wantOK)
		}
	}
}
