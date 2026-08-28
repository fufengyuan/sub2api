//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/scheduler"
)

// fakeCnRepo 账号仓储切片 stub（ListByPlatform + GetByID + Update）。
type fakeCnRepo struct {
	accounts map[int64]*Account
	updated  []*Account
}

func (f *fakeCnRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	var out []Account
	for _, a := range f.accounts {
		if a.Platform == platform {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeCnRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	a, ok := f.accounts[id]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := *a
	return &cp, nil
}

func (f *fakeCnRepo) Update(ctx context.Context, account *Account) error {
	f.updated = append(f.updated, account)
	return nil
}

// fakeCnUpstream provider.Upstream stub：仅积分/签到/模型三组方法可配置。
type fakeCnUpstream struct {
	remain      int64
	remainErr   error
	items       []provider.ResourceItem
	detailErr   error
	checkinErr  error
	models      []provider.ModelInfo
	modelsErr   error
	checkinCall int
}

func (f *fakeCnUpstream) RefreshToken(a *auth.Auth) error { return nil }
func (f *fakeCnUpstream) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, errors.New("not implemented")
}
func (f *fakeCnUpstream) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	return f.models, f.modelsErr
}
func (f *fakeCnUpstream) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (f *fakeCnUpstream) UserResource(a *auth.Auth) (int64, error) {
	return f.remain, f.remainErr
}
func (f *fakeCnUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return f.remain, f.items, f.detailErr
}
func (f *fakeCnUpstream) DailyCheckin(a *auth.Auth) error {
	f.checkinCall++
	return f.checkinErr
}
func (f *fakeCnUpstream) Classify(status int, body string) provider.ErrKind {
	return provider.ErrNone
}
func (f *fakeCnUpstream) Stream(w http.ResponseWriter, r io.Reader) error { return nil }
func (f *fakeCnUpstream) Aggregate(r io.Reader) (map[string]any, error) {
	return nil, nil
}

// newCnUpstreamTestSvc 构造替换过 upstream stub 的服务。
func newCnUpstreamTestSvc(repo *fakeCnRepo, up *fakeCnUpstream) *CnUpstreamService {
	svc := NewCnUpstreamService(repo, nil)
	for platform := range svc.platforms {
		svc.platforms[platform].upstream = up
	}
	return svc
}

func cnTestAccount(id int64, platform string) *Account {
	return &Account{
		ID:          id,
		Platform:    platform,
		Name:        "t-" + platform,
		Credentials: map[string]any{"accessToken": "at"},
	}
}

func TestCnUpstreamRefreshCredits(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{7: cnTestAccount(7, PlatformWorkBuddy)}}
	up := &fakeCnUpstream{remain: 4321}
	svc := newCnUpstreamTestSvc(repo, up)

	remain, err := svc.RefreshCredits(context.Background(), 7)
	if err != nil {
		t.Fatalf("RefreshCredits returned error: %v", err)
	}
	if remain != 4321 {
		t.Fatalf("remain = %d, want 4321", remain)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("expected account persisted once, got %d", len(repo.updated))
	}
	got := repo.updated[0].Credentials["creditsRemain"]
	if n, ok := got.(int64); !ok || n != 4321 {
		t.Fatalf("credentials.creditsRemain = %v (%T), want int64 4321", got, got)
	}
}

func TestCnUpstreamResourceDetail(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{8: cnTestAccount(8, PlatformTraeWork)}}
	up := &fakeCnUpstream{
		remain: 100,
		items:  []provider.ResourceItem{{Name: "免费套餐", Total: 100, Used: 30, Remain: 70}},
	}
	svc := newCnUpstreamTestSvc(repo, up)

	detail, err := svc.ResourceDetail(context.Background(), 8)
	if err != nil {
		t.Fatalf("ResourceDetail returned error: %v", err)
	}
	if detail.CreditsRemain != 100 || len(detail.Items) != 1 || detail.Items[0].Remain != 70 {
		t.Fatalf("detail = %+v, want remain=100 items[0].remain=70", detail)
	}
	// 明细实时查询，不落库。
	if len(repo.updated) != 0 {
		t.Fatalf("detail must not persist, got %d updates", len(repo.updated))
	}
}

func TestCnUpstreamCheckinNow_Success(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{9: cnTestAccount(9, PlatformWorkBuddy)}}
	up := &fakeCnUpstream{remain: 555}
	svc := newCnUpstreamTestSvc(repo, up)

	result, err := svc.CheckinNow(context.Background(), 9)
	if err != nil {
		t.Fatalf("CheckinNow returned error: %v", err)
	}
	if !result.Success || result.CreditsRemain != 555 {
		t.Fatalf("result = %+v, want success with remain=555", result)
	}
	if up.checkinCall != 1 {
		t.Fatalf("checkin called %d times, want 1", up.checkinCall)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("expected credits persisted after checkin, got %d updates", len(repo.updated))
	}
}

func TestCnUpstreamCheckinNow_QoderNoActivity(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{11: cnTestAccount(11, PlatformQoder)}}
	up := &fakeCnUpstream{checkinErr: errors.New("qoder 无签到活动")}
	svc := newCnUpstreamTestSvc(repo, up)

	result, err := svc.CheckinNow(context.Background(), 11)
	if err != nil {
		t.Fatalf("business failure must not return error: %v", err)
	}
	if result.Success {
		t.Fatalf("qoder checkin should fail, got %+v", result)
	}
}

func TestCnUpstreamUnsupportedPlatform(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{12: cnTestAccount(12, "anthropic")}}
	svc := newCnUpstreamTestSvc(repo, &fakeCnUpstream{})

	if _, err := svc.RefreshCredits(context.Background(), 12); err == nil {
		t.Fatal("non-cn platform should fail")
	}
	if _, err := svc.CheckinNow(context.Background(), 12); err == nil {
		t.Fatal("non-cn platform should fail")
	}
	if _, err := svc.FetchUpstreamModels(context.Background(), 12); err == nil {
		t.Fatal("non-cn platform should fail")
	}
}

func TestCnUpstreamFetchUpstreamModels(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{13: cnTestAccount(13, PlatformQoder)}}
	up := &fakeCnUpstream{models: []provider.ModelInfo{{ID: "qoder-max", Name: "Qoder Max"}}}
	svc := newCnUpstreamTestSvc(repo, up)

	models, err := svc.FetchUpstreamModels(context.Background(), 13)
	if err != nil {
		t.Fatalf("FetchUpstreamModels returned error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "qoder-max" {
		t.Fatalf("models = %+v, want [qoder-max]", models)
	}
}

// PersistAuth 把刷新后的凭证按 uid 回写 ent credentials，
// 且必须保留既有业务键（model_mapping/creditsRemain 等，整行覆盖不可抹掉）。
func TestCnUpstreamPersistAuth(t *testing.T) {
	acct := cnTestAccount(14, PlatformWorkBuddy)
	acct.Credentials["model_mapping"] = map[string]any{"gpt-x": "gpt-y"}
	acct.Credentials["creditsRemain"] = int64(77)
	repo := &fakeCnRepo{accounts: map[int64]*Account{14: acct}}
	svc := newCnUpstreamTestSvc(repo, &fakeCnUpstream{})
	if err := svc.ReloadAccounts(context.Background()); err != nil {
		t.Fatalf("ReloadAccounts: %v", err)
	}

	a := &auth.Auth{UID: "14", AccessToken: "new-at", RefreshToken: "new-rt", ExpiresAt: 123}
	if err := svc.PersistAuth(a); err != nil {
		t.Fatalf("PersistAuth: %v", err)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updated))
	}
	creds := repo.updated[0].Credentials
	if creds["accessToken"] != "new-at" || creds["refreshToken"] != "new-rt" || creds["expiresAt"] != int64(123) {
		t.Fatalf("credentials = %v", creds)
	}
	if creds["model_mapping"] == nil {
		t.Fatalf("model_mapping must survive PersistAuth, got %v", creds)
	}
	if n, _ := creds["creditsRemain"].(int64); n != 77 {
		t.Fatalf("creditsRemain must survive PersistAuth, got %v", creds["creditsRemain"])
	}
}

// PersistCheckinResult 落库 lastCheckinAt/lastCheckinResult/creditsRemain 并同步池。
func TestCnUpstreamPersistCheckinResult(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{15: cnTestAccount(15, PlatformTraeWork)}}
	svc := newCnUpstreamTestSvc(repo, &fakeCnUpstream{})
	if err := svc.ReloadAccounts(context.Background()); err != nil {
		t.Fatalf("ReloadAccounts: %v", err)
	}

	svc.PersistCheckinResult(PlatformTraeWork, scheduler.CheckinResult{
		UID: "15", OK: true, Msg: "ok", Remain: 888, HasRemain: true,
	})
	if len(repo.updated) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updated))
	}
	creds := repo.updated[0].Credentials
	if creds["creditsRemain"] != int64(888) {
		t.Fatalf("creditsRemain = %v, want 888", creds["creditsRemain"])
	}
	if at, _ := creds["lastCheckinAt"].(string); at == "" {
		t.Fatalf("lastCheckinAt missing: %v", creds["lastCheckinAt"])
	}
	if res, _ := creds["lastCheckinResult"].(string); res != "ok" {
		t.Fatalf("lastCheckinResult = %v, want ok", creds["lastCheckinResult"])
	}
	if okv, _ := creds["lastCheckinOK"].(bool); !okv {
		t.Fatalf("lastCheckinOK = %v, want true", creds["lastCheckinOK"])
	}
	st, ok := svc.PlatformPool(PlatformTraeWork).Status("15")
	if !ok || st.Credits != 888 {
		t.Fatalf("pool credits = %+v, want 888", st)
	}
}

// 失败签到也落 lastCheckin 状态（展示失败原因），但不写余额。
func TestCnUpstreamPersistCheckinResult_Failure(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{16: cnTestAccount(16, PlatformWorkBuddy)}}
	svc := newCnUpstreamTestSvc(repo, &fakeCnUpstream{})
	if err := svc.ReloadAccounts(context.Background()); err != nil {
		t.Fatalf("ReloadAccounts: %v", err)
	}

	svc.PersistCheckinResult(PlatformWorkBuddy, scheduler.CheckinResult{
		UID: "16", OK: false, Msg: "session expired", HasRemain: false,
	})
	if len(repo.updated) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updated))
	}
	creds := repo.updated[0].Credentials
	if res, _ := creds["lastCheckinResult"].(string); res != "session expired" {
		t.Fatalf("lastCheckinResult = %v, want failure msg", creds["lastCheckinResult"])
	}
	if okv, _ := creds["lastCheckinOK"].(bool); okv {
		t.Fatalf("lastCheckinOK = %v, want false", creds["lastCheckinOK"])
	}
	if _, exists := creds["creditsRemain"]; exists {
		t.Fatalf("creditsRemain should not be touched on failure without remain: %v", creds)
	}
}

// uid 不在 acctRefs 时按 uid 解析账号 ID 重建引用（对齐 RefreshToken 兜底）。
func TestCnUpstreamPersistCheckinResult_UnknownUID(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{17: cnTestAccount(17, PlatformWorkBuddy)}}
	svc := newCnUpstreamTestSvc(repo, &fakeCnUpstream{})

	svc.PersistCheckinResult(PlatformWorkBuddy, scheduler.CheckinResult{
		UID: "17", OK: true, Msg: "ok", Remain: 66, HasRemain: true,
	})
	if len(repo.updated) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updated))
	}
	if repo.updated[0].ID != 17 {
		t.Fatalf("updated account ID = %d, want 17", repo.updated[0].ID)
	}
	if repo.updated[0].Credentials["creditsRemain"] != int64(66) {
		t.Fatalf("creditsRemain = %v", repo.updated[0].Credentials["creditsRemain"])
	}
}

// 手动签到 CheckinNow 成功：写 lastCheckin 状态（与自动签到口径一致）。
func TestCnUpstreamCheckinNow_WritesLastCheckin(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{18: cnTestAccount(18, PlatformWorkBuddy)}}
	up := &fakeCnUpstream{remain: 555}
	svc := newCnUpstreamTestSvc(repo, up)

	res, err := svc.CheckinNow(context.Background(), 18)
	if err != nil {
		t.Fatalf("CheckinNow: %v", err)
	}
	if !res.Success || res.Message != "ok" {
		t.Fatalf("result = %+v, want Success=true/message=ok", res)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updated))
	}
	creds := repo.updated[0].Credentials
	if at, _ := creds["lastCheckinAt"].(string); at == "" {
		t.Fatalf("lastCheckinAt missing: %v", creds["lastCheckinAt"])
	}
	if res, _ := creds["lastCheckinResult"].(string); res != "ok" {
		t.Fatalf("lastCheckinResult = %v, want ok", creds["lastCheckinResult"])
	}
	if okv, _ := creds["lastCheckinOK"].(bool); !okv {
		t.Fatalf("lastCheckinOK = %v, want true", creds["lastCheckinOK"])
	}
}

// 手动签到遇「已签到」：对齐原版 checkinOne 视为成功（Success=true/已签到），
// 并落 lastCheckinOK=true，避免前端误判失败。
func TestCnUpstreamCheckinNow_AlreadyCheckedIn_IsSuccess(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{19: cnTestAccount(19, PlatformTraeWork)}}
	up := &fakeCnUpstream{checkinErr: errors.New("今日已签到"), remain: 999}
	svc := newCnUpstreamTestSvc(repo, up)

	res, err := svc.CheckinNow(context.Background(), 19)
	if err != nil {
		t.Fatalf("CheckinNow: %v", err)
	}
	if !res.Success {
		t.Fatalf("已签到应视为成功，got %+v", res)
	}
	if res.Message != "已签到" {
		t.Fatalf("message = %q, want 已签到", res.Message)
	}
	if res.CreditsRemain != 999 {
		t.Fatalf("CreditsRemain = %d, want 999", res.CreditsRemain)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updated))
	}
	creds := repo.updated[0].Credentials
	if res2, _ := creds["lastCheckinResult"].(string); res2 != "已签到" {
		t.Fatalf("lastCheckinResult = %v, want 已签到", creds["lastCheckinResult"])
	}
	if okv, _ := creds["lastCheckinOK"].(bool); !okv {
		t.Fatalf("lastCheckinOK = %v, want true (已签到视为成功)", creds["lastCheckinOK"])
	}
}

// 手动签到真实失败（非已签到）：Success=false 且落 lastCheckinOK=false。
func TestCnUpstreamCheckinNow_Failure_IsNotSuccess(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{20: cnTestAccount(20, PlatformWorkBuddy)}}
	up := &fakeCnUpstream{checkinErr: errors.New("login expired")}
	svc := newCnUpstreamTestSvc(repo, up)

	res, err := svc.CheckinNow(context.Background(), 20)
	if err != nil {
		t.Fatalf("CheckinNow: %v", err)
	}
	if res.Success {
		t.Fatalf("真实失败应 Success=false, got %+v", res)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updated))
	}
	if okv, _ := repo.updated[0].Credentials["lastCheckinOK"].(bool); okv {
		t.Fatalf("lastCheckinOK = %v, want false", repo.updated[0].Credentials["lastCheckinOK"])
	}
}
