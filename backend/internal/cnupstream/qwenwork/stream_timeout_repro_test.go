package qwenwork

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChatStreamNotCutByClientTotalTimeout 复现「流式响应被 http.Client.Timeout 总超时
// 掐断」缺陷：SSE 流只要持续超过 client.Timeout（当前 qwenwork 默认 180s）就会被
// resp.Body 读取强制中断，长推理/长输出请求必炸。
//
// 用短超时（300ms）+ 持续 800ms 的 SSE 流放大问题：修复前 ChatStream 走
// c.HTTP{Timeout:300ms}，读到 300ms 时被 "context deadline exceeded" 掐断，读不全；
// 修复后流式走独立的无总超时 StreamHTTP，能完整读完整个流。
func TestChatStreamNotCutByClientTotalTimeout(t *testing.T) {
	const chunkCount = 16
	const chunkInterval = 50 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	// 用短超时的 NewWithTimeout 构造（修复后 StreamHTTP 由它正确初始化为无总超时）：
	// 普通 HTTP client 的 http.Client.Timeout 包含读 body 的总时长，流式超过它必被
	// 掐断；流式必须走独立的无超时 StreamHTTP（类比 traework）。
	c := NewWithTimeout(300 * time.Millisecond)
	c.Gateway = srv.URL
	rc, status, respBody, err := c.ChatStream(testAuth(), []byte(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}]}`))
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

	// 期望读到全部 chunk。修复前读到 300ms 时被掐断，读不满 chunkCount。
	for i := 0; i < chunkCount; i++ {
		want := fmt.Sprintf(`chunk%d`, i)
		if !strings.Contains(got, want) {
			t.Fatalf("流式响应不完整：缺少 chunk#%d（共 16 个）。缺陷：http.Client.Timeout 总超时掐断了长流。got=%q", i, got)
		}
	}
}
