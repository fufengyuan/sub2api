package provider

import (
	"regexp"
	"strconv"
	"strings"
)

// 三渠道（workbuddy / traework / qoder）共用的上游业务错误码分类表。
//
// 业务码出现在两个位置：HTTP 4xx 响应体的 code 字段，以及 SSE event:error 帧
// 的 code。历史实现只按 HTTP 状态码分类，业务码一律落 ErrClient（要累计 3 次
// 错误才冷却），流内错误更是完全不回灌池状态机——被限流的账号下一次仍会按
// 「池内积分最高」被优先选中，用户侧表现为同一个限流错误反复出现、无法自愈。
//
// 分类口径按「换号 / 等待能否自愈」划分：
//   - 4008 / 4028：请求频次或配额超限（Trae SOLO "Your requests have exceeded
//     the quota."），间歇性，换账号或稍后重试即可恢复 → ErrSoftRate（短冷却 + 换号）
//   - 1005：权益 / 订阅不足，账号级不可自愈 → ErrHardCredit（长冷却）
//   - 1001 / 4001：模型名或能力不匹配，换号也无济于事 → ErrClient（错误计数）
var businessCodeKinds = map[int64]ErrKind{
	1001: ErrClient,
	1005: ErrHardCredit,
	4001: ErrClient,
	4008: ErrSoftRate,
	4028: ErrSoftRate,
	9074: ErrSoftRate,
}

// softRateMarkers 文案兜底：码表未覆盖的新码，只要文案表明是频控/配额超限，
// 一律按软限流处理（短冷却 + 换号），避免把可恢复的间歇限流误判成账号故障。
var softRateMarkers = []string{
	"too frequent", "too many requests", "rate limit", "ratelimit",
	"exceeded the quota", "quota limit", "operation too frequent",
	"频繁", "限流", "稍后再试", "请稍后",
}

// ClassifyBusinessCode 按上游业务码 + 文案分类错误。未知码落 ErrClient，
// 由调用方（池）按错误计数处理，不做长冷却——宁可多重试一次，也不要把
// 一个只是文案变化的可恢复错误锁死账号 30 分钟。
func ClassifyBusinessCode(code int64, msg string) ErrKind {
	if kind, ok := businessCodeKinds[code]; ok {
		return kind
	}
	lower := strings.ToLower(msg)
	for _, marker := range softRateMarkers {
		if strings.Contains(lower, marker) || strings.Contains(msg, marker) {
			return ErrSoftRate
		}
	}
	return ErrClient
}

// businessCodeRe 从上游响应体中提取业务码，兼容 {"code":4028}、{"code":"4028"}
// 以及 Trae OAuth 信封的大写键写法 {"Code":4028}。键名整体大小写不敏感。
var businessCodeRe = regexp.MustCompile(`(?i)"code"\s*:\s*"?(-?\d+)`)

// ExtractBusinessCode 从原始响应体里提取首个业务码；无码时返回 (0, false)。
func ExtractBusinessCode(body string) (int64, bool) {
	matches := businessCodeRe.FindStringSubmatch(body)
	if len(matches) < 2 {
		return 0, false
	}
	code, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return code, true
}
