package qwenwork

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUserResourceDetailFallsBackTotalWhenUpstreamOmitsTotalUsed
// 回归：千问办公 account-context 真实上游只返回 quota.remaining（不提供
// total/used）。此前 UserResourceDetail 在 total/used 缺失时置 0，导致积分明细
// 弹窗显示「总量=0、已用=0」误导。修复后 total 应反推为 remaining（当前可用额度）。
func TestUserResourceDetailFallsBackTotalWhenUpstreamOmitsTotalUsed(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		// 只给 remaining，不给 total/used（模拟真实上游返回）。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"quota": map[string]any{
					"remaining": float64(2091),
				},
			},
		})
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), Gateway: srv.URL}
	a := testAuth()

	remain, items, err := c.UserResourceDetail(a)
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 2091 {
		t.Fatalf("remain = %d, want 2091（remaining 应正常解析）", remain)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	item := items[0]
	if item.Remain != 2091 {
		t.Errorf("item.Remain = %d, want 2091", item.Remain)
	}
	if item.Total != 2091 {
		t.Errorf("item.Total = %d, want 2091（total 缺失时应反推为 remaining，而非 0）", item.Total)
	}
	if item.Used != 0 {
		t.Errorf("item.Used = %d, want 0（used 缺失时保持 0）", item.Used)
	}
	if gotURI != EpAccountContext {
		t.Errorf("request uri = %q, want %q", gotURI, EpAccountContext)
	}
}
