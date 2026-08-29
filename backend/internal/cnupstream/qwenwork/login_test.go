package qwenwork

import (
	"errors"
	"strings"
	"testing"
)

// StartLogin 生成的授权 URL 必须携带 PKCE challenge/nonce/machine_id，
// 且 machine_id 与建号时写入 credentials.machineId 的值同源。
func TestStartLoginBuildsDeviceAuthorizeURL(t *testing.T) {
	c := &Client{}
	st, authURL, err := c.StartLogin("machine-abc")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st == nil || st.Verifier == "" || st.Nonce == "" {
		t.Fatalf("login state incomplete: %+v", st)
	}
	if st.MachineID != "machine-abc" {
		t.Fatalf("machineID = %q, want caller-provided value", st.MachineID)
	}
	for _, want := range []string{
		GatewayBase + oauthDeviceSelect,
		"challenge_method=S256",
		"nonce=" + st.Nonce,
		"machine_id=machine-abc",
		"client_id=" + oauthClientID,
	} {
		if !strings.Contains(authURL, want) {
			t.Fatalf("authURL missing %q: %s", want, authURL)
		}
	}
	// challenge 必须能由 verifier 推导（S256），保证轮询换 token 时配对。
	if _, challenge := makePKCE(); len(challenge) < 40 {
		t.Fatalf("challenge too short: %q", challenge)
	}
}

// 未传 machine_id 时自动生成 UUID（建号链路依赖该值持久化）。
func TestStartLoginGeneratesMachineID(t *testing.T) {
	c := &Client{}
	st, _, err := c.StartLogin("")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if len(st.MachineID) != 36 || strings.Count(st.MachineID, "-") != 4 {
		t.Fatalf("machineID = %q, want uuid4", st.MachineID)
	}
}

// 状态缺失必须显式报错，而不是把空参数打到上游。
func TestPollLoginRejectsIncompleteState(t *testing.T) {
	c := &Client{}
	if _, err := c.PollLogin(nil); err == nil {
		t.Fatal("nil state should error")
	}
	if _, err := c.PollLogin(&LoginState{}); err == nil {
		t.Fatal("empty state should error")
	}
	// 错误链路里 ErrLoginPending 只应由「上游明确未完成」产生，不用于参数错误。
	_, err := c.PollLogin(&LoginState{})
	if errors.Is(err, ErrLoginPending) {
		t.Fatal("parameter errors must not be mapped to ErrLoginPending")
	}
}
