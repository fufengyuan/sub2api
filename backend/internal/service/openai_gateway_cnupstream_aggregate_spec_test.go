//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNormalizeCnAggregateResponse_BackfillsModel
// 回归:traework 的 Aggregate 里 model 恒为 ""(上游不返回模型名),客户端收到
// model:"" 会让部分 SDK 的响应校验/展示异常。必须回填为客户端可见的模型名。
func TestNormalizeCnAggregateResponse_BackfillsModel(t *testing.T) {
	t.Run("空 model 回填计费模型名", func(t *testing.T) {
		agg := map[string]any{"model": "", "object": "chat.completion"}
		normalizeCnAggregateResponse(agg, "deepseek-v4-flash-upstream", "deepseek-v4-flash")
		require.Equal(t, "deepseek-v4-flash-upstream", agg["model"])
	})

	t.Run("计费模型名为空时回退到请求模型名", func(t *testing.T) {
		agg := map[string]any{"model": "  "}
		normalizeCnAggregateResponse(agg, "", "deepseek-v4-flash")
		require.Equal(t, "deepseek-v4-flash", agg["model"])
	})

	t.Run("已有非空 model 不覆盖", func(t *testing.T) {
		agg := map[string]any{"model": "qwen3-max"}
		normalizeCnAggregateResponse(agg, "billing", "requested")
		require.Equal(t, "qwen3-max", agg["model"])
	})
}

// TestNormalizeCnAggregateResponse_UsageAlwaysPresent
// 回归:上游未下发 token_usage 时聚合结果没有 usage 字段,而 OpenAI 规范里 usage
// 是恒定字段。缺字段会让客户端取到 undefined。
func TestNormalizeCnAggregateResponse_UsageAlwaysPresent(t *testing.T) {
	t.Run("缺 usage 时补零值", func(t *testing.T) {
		agg := map[string]any{"model": "m"}
		normalizeCnAggregateResponse(agg, "", "")
		u, ok := agg["usage"].(map[string]any)
		require.True(t, ok, "usage 字段必须存在")
		require.Equal(t, 0, u["prompt_tokens"])
		require.Equal(t, 0, u["completion_tokens"])
		require.Equal(t, 0, u["total_tokens"])
	})

	t.Run("已有 usage 不覆盖", func(t *testing.T) {
		agg := map[string]any{"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2}}
		normalizeCnAggregateResponse(agg, "", "")
		u := agg["usage"].(map[string]any)
		require.Equal(t, 5, u["prompt_tokens"])
	})
}

// TestNormalizeCnAggregateResponse_ChoicesShape
// 回归:choices[0] 必须带 index / finish_reason,message 必须带 role。
// 聚合失败被上层置成空 map 时也要产出合法的最小响应。
func TestNormalizeCnAggregateResponse_ChoicesShape(t *testing.T) {
	t.Run("空 map 兜底成合法响应", func(t *testing.T) {
		agg := map[string]any{}
		normalizeCnAggregateResponse(agg, "", "gpt-5")
		require.Equal(t, "chat.completion", agg["object"])
		choices, ok := agg["choices"].([]any)
		require.True(t, ok)
		require.Len(t, choices, 1)
		c := choices[0].(map[string]any)
		require.Equal(t, 0, c["index"])
		require.Equal(t, "stop", c["finish_reason"])
		msg := c["message"].(map[string]any)
		require.Equal(t, "assistant", msg["role"])
	})

	t.Run("补齐缺失的 index/finish_reason/role", func(t *testing.T) {
		agg := map[string]any{
			"object":  "chat.completion",
			"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
		}
		normalizeCnAggregateResponse(agg, "", "")
		c := agg["choices"].([]any)[0].(map[string]any)
		require.Equal(t, 0, c["index"])
		require.Equal(t, "stop", c["finish_reason"])
		require.Equal(t, "assistant", c["message"].(map[string]any)["role"])
	})

	t.Run("输出可被 JSON 序列化且结构合法", func(t *testing.T) {
		agg := map[string]any{}
		normalizeCnAggregateResponse(agg, "", "gpt-5")
		b, err := json.Marshal(agg)
		require.NoError(t, err)
		var back map[string]any
		require.NoError(t, json.Unmarshal(b, &back))
		require.Equal(t, "chat.completion", back["object"])
		require.Equal(t, "gpt-5", back["model"])
	})
}
