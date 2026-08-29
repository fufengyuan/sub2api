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
		{"1001 模型不可用不换号", 1001, "model not available", ErrClient},
		{"4001 模型名错误不换号", 4001, "invalid model name", ErrClient},
		{"未知码命中限流文案按软限流", 7000, "Rate limit exceeded, 请稍后再试", ErrSoftRate},
		{"未知码无文案兜底按客户端错误", 7001, "", ErrClient},
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
