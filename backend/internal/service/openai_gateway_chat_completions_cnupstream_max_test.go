//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureTraeWorkMaxSuffix
// 回归:TraeWork 上游用 "__max" 后缀标记 Max 1M 上下文能力(经抓包确认无独立
// 上下文字段)。网关转发 traework 平台时自动追加该后缀,但客户端已带时必须幂等,
// 避免拼成 xxx__max__max。
func TestEnsureTraeWorkMaxSuffix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "裸模型名补后缀", in: "DeepSeek-V4-Flash-Official", want: "DeepSeek-V4-Flash-Official__max"},
		{name: "已带后缀保持幂等", in: "DeepSeek-V4-Flash-Official__max", want: "DeepSeek-V4-Flash-Official__max"},
		{name: "大小写精确(不误判大写下划线变体)", in: "gpt-5__Max", want: "gpt-5__Max__max"},
		{name: "空串补成后缀", in: "", want: "__max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ensureTraeWorkMaxSuffix(tc.in))
		})
	}
}
