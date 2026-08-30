package upstream

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
)

// TestChatStreamNotCutByClientTotalTimeout 复现 workbuddy 上游流式被
// http.Client.Timeout 总超时掐断的缺陷：ChatStream 走 c.HTTP{Timeout:120s}，
// SSE 流一旦持续超过它就会被 resp.Body 读取强制中断（长推理/长输出必炸）。
//
// 把 HTTP 的超时放大成 300ms、SSE 流持续 800ms：修复前 ChatStream 走
// c.HTTP，300ms 时被 "context deadline exceeded" 掐断读不全；修复后流式走
// 独立的无总超时 StreamHTTP（与 traework/qwenwork/qoder 同范式），能完整
// 读完整个流。
func TestChatStreamNotCutByClientTotalTimeout(t *testing.T) {
	const chunkCount = 16
	const chunkInterval = 50 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v2/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunkCount; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk%d\"}}]}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(chunkInterval)
		}
	}))
	defer srv.Close()

	// New() 修复后会把 StreamHTTP 初始化为无总超时客户端；这里只把 HTTP 换成
	// 短超时版（模拟生产 120s 总超时被放大），StreamHTTP 保持 New() 的默认值。
	c := New()
	c.HTTP = &http.Client{Timeout: 300 * time.Millisecond, Transport: c.HTTP.Transport}
	c.ChatBaseCN = srv.URL

	rc, status, respBody, err := c.ChatStream(&auth.Auth{UID: "uid-test", AccessToken: "at-test"}, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil {
		t.Fatalf("ChatStream transport error: %v", err)
	}
	if status >= 400 {
		t.Fatalf("ChatStream upstream status = %d body=%s", status, respBody)
	}
	if rc != nil {
		defer rc.Close()
	}

	got := ""
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "data:") {
			got += line
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读取 SSE 流被中断（疑似 http.Client.Timeout 总超时掐断）: %v", err)
	}
	for i := 0; i < chunkCount; i++ {
		want := fmt.Sprintf(`chunk%d`, i)
		if !strings.Contains(got, want) {
			t.Fatalf("流式响应不完整：缺少 chunk#%d（共 %d 个）。缺陷：http.Client.Timeout 总超时掐断了长流。got=%q", i, chunkCount, got)
		}
	}
}
