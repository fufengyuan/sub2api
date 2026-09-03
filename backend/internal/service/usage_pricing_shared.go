package service

import (
	"errors"
	"strings"
)

// isUsagePricingUnavailableError 判断错误是否为「无价可循」：模型在内置价卡、
// 分组/渠道定价里都没有条目，无法计算成本。
//
// 这类错误不是系统故障——请求已经成功完成，只是计费侧缺价。两条网关路径
// （旧 GatewayService 与新 OpenAIGatewayService）共用此判定，保证行为一致：
// 零成本落账 + WARN 日志，绝不能因为缺价就丢弃 usage 记录或中断请求。
func isUsagePricingUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrModelPricingUnavailable) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no pricing available") || strings.Contains(msg, "pricing not found")
}
