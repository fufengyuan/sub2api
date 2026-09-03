package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// cnCreditPricingDefaultRate 模型未在倍率表中收录时使用的保守倍率。
//
// 上游 /models 接口返回的模型列表可能滞后于实际上线的新模型（如 qwen3.8-flash），
// 此时按 1x 折算：既不会像零成本那样让用户白嫖，也不会像猜测高价那样误扣。
// 运营者发现后可补充倍率表或临时调整该常量。
const cnCreditPricingDefaultRate = 1.0

// cnCreditRateCacheTTL 模型倍率缓存时长。倍率随上游套餐/折扣变动，但变化频率
// 远低于请求频率；短 TTL 保证调价后能较快生效。
const cnCreditRateCacheTTL = 5 * time.Minute

// cnCreditRateCache 按「平台 + 账号」缓存上游模型积分倍率，避免每个请求都打
// 上游 /models 接口（该接口是账号级鉴权，无法全局共享）。
type cnCreditRateCache struct {
	mu      sync.RWMutex
	entries map[string]cnCreditRateEntry
}

type cnCreditRateEntry struct {
	rates     map[string]float64
	expiresAt time.Time
}

func newCnCreditRateCache() *cnCreditRateCache {
	return &cnCreditRateCache{entries: make(map[string]cnCreditRateEntry)}
}

// lookup 查询模型倍率；命中缓存且未过期返回 (rate, true)。
func (c *cnCreditRateCache) lookup(key, model string) (float64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return 0, false
	}
	rate, ok := entry.rates[model]
	return rate, ok
}

// store 写入整份倍率表快照。
func (c *cnCreditRateCache) store(key string, rates map[string]float64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cnCreditRateEntry{rates: rates, expiresAt: time.Now().Add(cnCreditRateCacheTTL)}
}

// cnCreditPricingEnabled 判断积分折算是否开启。必须同时满足：配置开启 + 换算
// 系数为正。缺任一项都退回「零成本 + WARN」的既有行为，绝不猜测定价。
func cnCreditPricingEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.CnCreditPricing.Enabled {
		return false
	}
	return cfg.CnCreditPricing.CreditToUSD > 0
}

// cnCreditRateForModel 解析模型的积分倍率：优先查缓存，未命中回退保守值 1x。
//
// 返回的 found=false 表示倍率表中确实没有该模型（而非查询失败），调用方据此
// 决定是否额外告警，便于运营者持续补齐价卡。
func cnCreditRateForModel(cache *cnCreditRateCache, key, model string) (rate float64, found bool) {
	if r, ok := cache.lookup(key, model); ok && r > 0 {
		return r, true
	}
	return cnCreditPricingDefaultRate, false
}

// cnCreditRateCacheKey 缓存键：平台 + 账号 ID。倍率接口是账号级鉴权，同一平台
// 不同账号可能因套餐差异返回不同倍率（尤其折扣价），因此不能只按平台缓存。
func cnCreditRateCacheKey(platform string, accountID int64) string {
	return platform + "|" + itoa64(accountID)
}

// cnCreditCost 按「积分倍率 → USD」折算成本。
//
// 国产上游按积分计费：每模型一个 Rate 表示每次请求消耗的积分数。总成本 =
// Rate × CreditToUSD × 倍率系数（分组/账号的 RateMultiplier）。
// 注意与 token 计费的量纲区别：这里不乘 token 数，因为 Rate 本身就是单次请求
// 的积分消耗量，上游已按输入/输出长度折算进 Rate 的计费口径。
func cnCreditCost(rate, creditToUSD, multiplier float64) float64 {
	if rate <= 0 || creditToUSD <= 0 {
		return 0
	}
	if multiplier <= 0 {
		// 倍率系数非正（配置异常或零值）时按 1x 处理，避免把成本乘成 0 而漏扣。
		multiplier = 1
	}
	return rate * creditToUSD * multiplier
}

// normalizeCnCreditModel 归一化模型名以匹配上游倍率表：上游返回的模型名大小写
// 与分隔符可能与客户端请求名不一致（如 "Qwen3.8-Max" vs "qwen3.8-max"）。
func normalizeCnCreditModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// refreshCnCreditRates 拉取并缓存指定账号的上游模型倍率表。
// 上游调用失败时静默降级（保留旧缓存或走 1x 兜底），不阻断计费链路。
func (s *CnUpstreamService) refreshCnCreditRates(ctx context.Context, platform string, accountID int64) map[string]float64 {
	if s == nil {
		return nil
	}
	rt, _, a, err := s.loadCnAccount(ctx, accountID)
	if err != nil {
		return nil
	}
	if pooled := rt.pool.AuthByAccountID(accountID); pooled != nil {
		a = pooled
	}
	pricings, err := rt.upstream.FetchModelPricing(a)
	if err != nil {
		return nil
	}
	rates := make(map[string]float64, len(pricings))
	for _, p := range pricings {
		if p.Rate <= 0 {
			continue
		}
		rates[normalizeCnCreditModel(p.Model)] = p.Rate
	}
	if len(rates) == 0 {
		return nil
	}
	if s.creditRateCache == nil {
		s.creditRateCache = newCnCreditRateCache()
	}
	s.creditRateCache.store(cnCreditRateCacheKey(platform, accountID), rates)
	return rates
}

// tryCnCreditCost 在内置 token 价卡缺失时，尝试按国产渠道的积分倍率折算成本。
//
// 返回 ok=false 表示不适用（非国产渠道 / 折算未开启 / 无可用倍率），调用方应
// 继续走零成本 + WARN 的既有路径；ok=true 表示已按积分成功计价。
//
// 设计要点：
//   - 仅对国产四渠道账号生效，其他平台一律不适用，避免误改既有计费口径；
//   - 折算开关与换算系数都来自配置，任一缺失即跳过，绝不猜测定价；
//   - 上游倍率查询失败静默降级为 1x 保守倍率，不阻断计费链路。
func (s *GatewayService) tryCnCreditCost(
	ctx context.Context,
	account *Account,
	billingModel string,
	multiplier float64,
) (*CostBreakdown, bool) {
	if s == nil || account == nil || s.cnUpstream == nil {
		return nil, false
	}
	if !IsCnUpstreamPlatform(account.Platform) {
		return nil, false
	}
	cfg := s.cfg
	if cfg == nil {
		cfg = s.cnUpstream.cfg
	}
	if !cnCreditPricingEnabled(cfg) {
		return nil, false
	}

	key := cnCreditRateCacheKey(account.Platform, account.ID)
	model := normalizeCnCreditModel(billingModel)
	rate, found := cnCreditRateForModel(s.cnUpstream.creditRateCache, key, model)
	if !found {
		// 缓存未命中：拉一次上游倍率表再查；仍未收录则按 1x 保守折算。
		if rates := s.cnUpstream.refreshCnCreditRates(ctx, account.Platform, account.ID); len(rates) > 0 {
			rate, found = cnCreditRateForModel(s.cnUpstream.creditRateCache, key, model)
		}
	}

	cost := cnCreditCost(rate, cfg.CnCreditPricing.CreditToUSD, multiplier)
	if cost <= 0 {
		return nil, false
	}
	if !found {
		// 模型未被上游倍率表收录：仍按 1x 计费（不让用户白嫖），但打点提示
		// 运营者补齐，避免长期按保守值计费而不自知。
		logger.L().With(
			zap.String("component", "service.gateway"),
			zap.String("billing_model", billingModel),
			zap.String("platform", account.Platform),
			zap.Int64("account_id", account.ID),
			zap.Float64("fallback_rate", rate),
		).Warn("cn_credit_pricing.rate_not_found_using_default")
	}
	return &CostBreakdown{
		ActualCost:  cost,
		TotalCost:   cost,
		BillingMode: string(BillingModeToken),
	}, true
}

// itoa64 避免为单次键构造引入 strconv 依赖，且键拼接在热路径上。
func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
