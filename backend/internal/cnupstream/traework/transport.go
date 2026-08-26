package traework

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// newUTLSTransport 返回一个使用 uTLS 模拟 Chrome TLS 指纹的 RoundTripper，
// 并按目标 host 选择 HTTP 协议：
//   - ug/oauth 域 (api.trae.cn)：上游强制 HTTP/2，用 http2.Transport；
//   - agent/chat 域 (trae-api-cn.mchost.guru)：上游只认 HTTP/1.1，用 http.Transport + uTLS 强制 h1。
func newUTLSTransport() http.RoundTripper {
	h2t := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			return dialUTLS(ctx, network, addr, cfg)
		},
	}
	// chat 域强制 HTTP/1.1：uTLS ALPN 只给 http/1.1，服务器只会协商 h1，http.Transport 走 HTTP/1.1
	h1t := &http.Transport{
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		TLSNextProto:        map[string]func(authority string, c *tls.Conn) http.RoundTripper{},
		IdleConnTimeout:     90 * time.Second,
	}
	h1t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialUTLSH1(ctx, network, addr, &tls.Config{})
	}
	return &routedTransport{ugH2: h2t, chatH1: h1t}
}

// routedTransport 按 host 分发请求到 ug(h2) 或 chat(h1) 两个 uTLS transport
type routedTransport struct {
	ugH2   http.RoundTripper
	chatH1 http.RoundTripper
}

func (rt *routedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if isUGHost(req.URL.Host) {
		return rt.ugH2.RoundTrip(req)
	}
	return rt.chatH1.RoundTrip(req)
}

func (rt *routedTransport) CloseIdleConnections() {
	if c, ok := rt.ugH2.(*http2.Transport); ok {
		c.CloseIdleConnections()
	}
	if c, ok := rt.chatH1.(*http.Transport); ok {
		c.CloseIdleConnections()
	}
}

func isUGHost(host string) bool {
	h := strings.ToLower(host)
	return strings.Contains(h, "api.trae.cn") || strings.Contains(h, "api.trae.com.cn")
}

func dialUTLS(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		raw.Close()
		return nil, err
	}
	u := utls.UClient(raw, &utls.Config{
		ServerName:         host,
		RootCAs:            cfg.RootCAs,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}, utls.HelloChrome_Auto)
	if err := u.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}
	return u, nil
}

// dialUTLSH1 同 dialUTLS，但强制 ALPN 仅 http/1.1（用于 chat/agent 域，该上游只认 HTTP/1.1）。
func dialUTLSH1(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		raw.Close()
		return nil, err
	}
	u := utls.UClient(raw, &utls.Config{
		ServerName:         host,
		RootCAs:            cfg.RootCAs,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		NextProtos:         []string{"http/1.1"},
	}, utls.HelloChrome_Auto)
	if err := u.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}
	return u, nil
}
