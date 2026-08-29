// Package qwenwork 封装千问办公（qwenwork.cn）上游协议：COSY 签名、
// QoderEncoding 编码、嵌套 SSE 解析，并实现 provider.Upstream 接口。
// 千问办公与 Qoder（qoder.com.cn）同源：协议/签名/端点一致，仅域名不同。
// 移植自 qoderwork2api（Sliverkiss），凭证模型改为 internal/auth.Auth。
package qwenwork

import (
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
)

// 上游域名与端点（CN only）。
// 千问办公无独立 OpenAPI 域名：业务 API（dt- Bearer）与推理网关（COSY 签名）共用 gateway.qwenwork.cn。
const (
	GatewayBase = "https://gateway.qwenwork.cn" // 推理网关 + 业务 API（COSY 签名 + dt- Bearer 通用）

	// 积分接口：千问办公的账户上下文直接带 quota（total/used/remaining），
	// 与 qoder 的 /api/v2/quota/usage（userQuota/addOnQuota）结构不同。
	EpAccountContext = "/api/v1/adapter/user/account-context?include=user,plan,quota"
	EpUserInfo       = "/api/v1/userinfo"
	EpDTRefresh      = "/api/v1/deviceToken/refresh"
	EpModels         = "/algo/api/v2/model/list"
	EpChat           = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common"

	clientUA = "Go-http-client/2.0"
)

// staticModelKeys 客户端模型名 → 上游 model key 静态兜底表。
// 与 qoderwork2api fallbackModelMap 对齐；优先用动态模型接口（COSY /algo/api/v2/model/list）。
var staticModelKeys = map[string]string{
	"auto":              "auto",
	"qwen3.8-max":       "qmodel_38max",
	"qwen3.7-max":       "qmodel_latest",
	"qwen3.7-plus":      "qmodel",
	"qwen3.7-flash":     "q37fmodel",
	"qwen3.6-flash":     "q36fmodel",
	"deepseek-v4-pro":   "dmodel",
	"deepseek-v4-flash": "dfmodel",
	"glm-5.3":           "gmodel",
	"glm-5.2":           "gm51model",
	"kimi-k2.7-code":    "kmodel",
	"minimax-m2.7":      "mmodel",
}

// StaticModels 返回 provider.ModelInfo 静态兜底（供 server.Runtime.StaticModels）。
func StaticModels() []provider.ModelInfo {
	keys := make([]string, 0, len(staticModelKeys))
	for k := range staticModelKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]provider.ModelInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, provider.ModelInfo{ID: k, ContextWindow: 180000})
	}
	return out
}

// ModelKey 返回客户端模型名对应的上游 model key；未知模型返回空。
func ModelKey(clientName string) string {
	return staticModelKeys[clientName]
}
