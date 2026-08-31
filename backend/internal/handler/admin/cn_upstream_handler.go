package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CnUpstreamHandler 三渠道（workbuddy/traework/qoder）账号级积分与签到端点。
//
//	POST /admin/accounts/:id/credits/refresh  刷新积分余额（写入账号 credentials）
//	GET  /admin/accounts/:id/credits/detail   积分明细（各套餐条目）
//	POST /admin/accounts/:id/checkin          立即签到（成功后自动刷新余额）
//	GET  /admin/accounts/:id/upstream-models  拉取上游真实模型列表（模型映射配置辅助）
//
// 仅三渠道平台账号可用，其他平台返回 400。
type CnUpstreamHandler struct {
	svc *service.CnUpstreamService
}

// NewCnUpstreamHandler 构造 CnUpstreamHandler。
func NewCnUpstreamHandler(svc *service.CnUpstreamService) *CnUpstreamHandler {
	return &CnUpstreamHandler{svc: svc}
}

// parseAccountID 解析路径参数账号 ID。
func parseAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return 0, false
	}
	return id, true
}

// RefreshCredits 刷新单账号积分余额。
func (h *CnUpstreamHandler) RefreshCredits(c *gin.Context) {
	accountID, ok := parseAccountID(c)
	if !ok {
		return
	}
	if h == nil || h.svc == nil {
		response.InternalError(c, "cn upstream service is not enabled")
		return
	}
	remain, err := h.svc.RefreshCredits(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"credits_remain": remain})
}

// CreditsDetail 查询单账号积分明细。
func (h *CnUpstreamHandler) CreditsDetail(c *gin.Context) {
	accountID, ok := parseAccountID(c)
	if !ok {
		return
	}
	if h == nil || h.svc == nil {
		response.InternalError(c, "cn upstream service is not enabled")
		return
	}
	detail, err := h.svc.ResourceDetail(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, detail)
}

// Checkin 单账号立即签到。业务失败（已签到/无签到活动）返回 success=false + message。
func (h *CnUpstreamHandler) Checkin(c *gin.Context) {
	accountID, ok := parseAccountID(c)
	if !ok {
		return
	}
	if h == nil || h.svc == nil {
		response.InternalError(c, "cn upstream service is not enabled")
		return
	}
	result, err := h.svc.CheckinNow(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// UpstreamModels 拉取三渠道账号的上游真实模型列表。
func (h *CnUpstreamHandler) UpstreamModels(c *gin.Context) {
	accountID, ok := parseAccountID(c)
	if !ok {
		return
	}
	if h == nil || h.svc == nil {
		response.InternalError(c, "cn upstream service is not enabled")
		return
	}
	models, err := h.svc.FetchUpstreamModels(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, models)
}

// parseCnCreditsFilter 从查询参数解析国产渠道积分筛选条件（与账号列表筛选对齐）。
func parseCnCreditsFilter(c *gin.Context) service.CnCreditsFilter {
	f := service.CnCreditsFilter{
		Platform:    strings.TrimSpace(c.Query("platform")),
		AccountType: strings.TrimSpace(c.Query("type")),
		Status:      strings.TrimSpace(c.Query("status")),
		PrivacyMode: strings.TrimSpace(c.Query("privacy_mode")),
	}
	f.Search = strings.TrimSpace(c.Query("search"))
	if len(f.Search) > 100 {
		f.Search = f.Search[:100]
	}
	if groupStr := strings.TrimSpace(c.Query("group")); groupStr != "" {
		if groupStr == "ungrouped" {
			f.GroupID = service.AccountListGroupUngrouped
		} else if id, err := strconv.ParseInt(groupStr, 10, 64); err == nil && id >= 0 {
			f.GroupID = id
		}
	}
	return f
}

// CreditsSummary 按列表筛选条件对满足条件的全部国产渠道账号积分求和（全量口径，跨页）。
//
//	GET /admin/accounts/cn-credits/summary
func (h *CnUpstreamHandler) CreditsSummary(c *gin.Context) {
	if h == nil || h.svc == nil {
		response.InternalError(c, "cn upstream service is not enabled")
		return
	}
	total, counted, err := h.svc.SummarizeCreditsByFilter(c.Request.Context(), parseCnCreditsFilter(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"total": total, "counted": counted})
}

// RefreshCreditsBulk 按列表筛选条件一键刷新满足条件的全部国产渠道账号积分，返回成功/失败数。
//
//	POST /admin/accounts/cn-credits/refresh
func (h *CnUpstreamHandler) RefreshCreditsBulk(c *gin.Context) {
	if h == nil || h.svc == nil {
		response.InternalError(c, "cn upstream service is not enabled")
		return
	}
	success, failed, err := h.svc.RefreshCreditsByFilter(c.Request.Context(), parseCnCreditsFilter(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"success": success, "failed": failed})
}
