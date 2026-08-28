//go:build unit

package service

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// recordingCnTestUpstream 记录 ChatStream 入参并返回可聚合的伪流，
// 基于已有 fakeCnUpstream（cnupstream_service_test.go）扩展。
type recordingCnTestUpstream struct {
	*fakeCnUpstream
	lastAuth *auth.Auth
	lastBody []byte
}

func (r *recordingCnTestUpstream) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	r.lastAuth = a
	r.lastBody = body
	return io.NopCloser(strings.NewReader("")), 200, nil, nil
}

func (r *recordingCnTestUpstream) Aggregate(io.Reader) (map[string]any, error) {
	return map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "pong"}}},
	}, nil
}

func withCnTestUpstream(platform string, up provider.Upstream) func() {
	prev, had := cnTestUpstreamBuilders[platform]
	cnTestUpstreamBuilders[platform] = func() provider.Upstream { return up }
	return func() {
		if had {
			cnTestUpstreamBuilders[platform] = prev
		} else {
			delete(cnTestUpstreamBuilders, platform)
		}
	}
}

func cnUpstreamOAuthTestAccount(id int64, platform string) *Account {
	return &Account{
		ID:          id,
		Name:        "cn-oauth-test",
		Platform:    platform,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"accessToken":  "at-camel",
			"refreshToken": "rt",
			"model_mapping": map[string]any{
				"claude-sonnet-4-5": "glm-tram-internal",
			},
		},
	}
}

// 回归：三渠道 oauth 账号测试连接必须直连 cnupstream 上游（携带该账号 camelCase
// accessToken、模型经映射转为真实模型），不得落入 Claude 分支报 No access token available。
func TestAccountTestService_CnUpstreamOAuthDirectChat(t *testing.T) {
	for _, platform := range []string{PlatformWorkBuddy, PlatformTraeWork, PlatformQoder} {
		t.Run(platform, func(t *testing.T) {
			account := cnUpstreamOAuthTestAccount(401, platform)
			repo := &openAIAccountTestRepo{
				mockAccountRepoForGemini: mockAccountRepoForGemini{
					accountsByID: map[int64]*Account{account.ID: account},
				},
			}
			fake := &recordingCnTestUpstream{fakeCnUpstream: &fakeCnUpstream{}}
			restore := withCnTestUpstream(platform, fake)
			defer restore()

			svc := &AccountTestService{accountRepo: repo}
			c, recorder := newTestContext()

			err := svc.TestAccountConnection(c, account.ID, "claude-sonnet-4-5", "hello", AccountTestModeDefault)

			require.NoError(t, err)
			require.NotNil(t, fake.lastAuth, "must call cn upstream with the account auth")
			require.Equal(t, "at-camel", fake.lastAuth.AccessToken)
			require.Equal(t, "glm-tram-internal", gjsonModel(fake.lastBody))
			body := recorder.Body.String()
			require.Contains(t, body, `"type":"content"`)
			require.Contains(t, body, "pong")
			require.Contains(t, body, `"type":"test_complete","success":true`)
			require.NotContains(t, body, "No access token available")
		})
	}
}

// 映射缺失时直接用请求模型名发上游（即账号本就使用真实模型名的场景）。
func TestAccountTestService_CnUpstreamWithoutMappingUsesRequestedModel(t *testing.T) {
	account := cnUpstreamOAuthTestAccount(402, PlatformWorkBuddy)
	delete(account.Credentials, "model_mapping")
	repo := &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{account.ID: account},
		},
	}
	fake := &recordingCnTestUpstream{fakeCnUpstream: &fakeCnUpstream{}}
	restore := withCnTestUpstream(PlatformWorkBuddy, fake)
	defer restore()

	svc := &AccountTestService{accountRepo: repo}
	c, _ := newTestContext()

	err := svc.TestAccountConnection(c, account.ID, "kimi-k2-instruct", "", AccountTestModeDefault)

	require.NoError(t, err)
	require.Equal(t, "kimi-k2-instruct", gjsonModel(fake.lastBody))
}

// 未选择模型时给出可读错误而非误打上游。
func TestAccountTestService_CnUpstreamRequiresModel(t *testing.T) {
	account := cnUpstreamOAuthTestAccount(403, PlatformTraeWork)
	repo := &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{account.ID: account},
		},
	}
	fake := &recordingCnTestUpstream{fakeCnUpstream: &fakeCnUpstream{}}
	restore := withCnTestUpstream(PlatformTraeWork, fake)
	defer restore()

	svc := &AccountTestService{accountRepo: repo}
	c, recorder := newTestContext()

	err := svc.TestAccountConnection(c, account.ID, "", "", AccountTestModeDefault)

	// sendErrorAndEnd 按项目惯例同时返回 error（与其他平台分支一致）。
	require.Error(t, err)
	require.Nil(t, fake.lastAuth)
	require.Contains(t, recorder.Body.String(), `"type":"error"`)
	require.NotContains(t, recorder.Body.String(), "No access token available")
}

func gjsonModel(body []byte) string {
	return gjson.GetBytes(body, "model").String()
}

// quotaErrorCnUpstream Aggregate 返回 SOLOStreamError（如 traework 4008 配额超限）。
type quotaErrorCnUpstream struct {
	*fakeCnUpstream
}

func (q *quotaErrorCnUpstream) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return io.NopCloser(strings.NewReader("")), 200, nil, nil
}

func (q *quotaErrorCnUpstream) Aggregate(io.Reader) (map[string]any, error) {
	return nil, &traework.SOLOStreamError{Code: 4008, Msg: "Your requests have exceeded the quota"}
}

// 回归：traework 配额超限（SOLOStreamError 4008）不应被误报为「模型名不对」，
// 应直接透传上游业务错误，避免误导用户去核对本已正确的模型名。
func TestAccountTestService_CnUpstreamQuotaErrorNotMisreportedAsModelName(t *testing.T) {
	account := cnUpstreamOAuthTestAccount(404, PlatformTraeWork)
	repo := &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{account.ID: account},
		},
	}
	up := &quotaErrorCnUpstream{fakeCnUpstream: &fakeCnUpstream{}}
	restore := withCnTestUpstream(PlatformTraeWork, up)
	defer restore()

	svc := &AccountTestService{accountRepo: repo}
	c, recorder := newTestContext()

	err := svc.TestAccountConnection(c, account.ID, "deepseek-v4-flash", "Hi", AccountTestModeDefault)

	require.Error(t, err)
	body := recorder.Body.String()
	require.Contains(t, body, `"type":"error"`)
	// 透传上游业务错误（4008 配额超限），不得出现「上游拒绝了模型」误导文案。
	require.Contains(t, body, "solo error code=4008")
	require.Contains(t, body, "exceeded the quota")
	require.NotContains(t, body, "上游拒绝了模型")
	require.NotContains(t, body, "请核对账号编辑弹窗")
}

// plainErrorCnUpstream Aggregate 返回普通错误（非 SOLOStreamError），应保留模型名核对提示。
type plainErrorCnUpstream struct {
	*fakeCnUpstream
}

func (p *plainErrorCnUpstream) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return io.NopCloser(strings.NewReader("")), 200, nil, nil
}

func (p *plainErrorCnUpstream) Aggregate(io.Reader) (map[string]any, error) {
	return nil, errors.New("model not found")
}

func TestAccountTestService_CnUpstreamPlainErrorKeepsModelNameHint(t *testing.T) {
	account := cnUpstreamOAuthTestAccount(405, PlatformTraeWork)
	repo := &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{account.ID: account},
		},
	}
	up := &plainErrorCnUpstream{fakeCnUpstream: &fakeCnUpstream{}}
	restore := withCnTestUpstream(PlatformTraeWork, up)
	defer restore()

	svc := &AccountTestService{accountRepo: repo}
	c, recorder := newTestContext()

	_ = svc.TestAccountConnection(c, account.ID, "some-model", "Hi", AccountTestModeDefault)

	body := recorder.Body.String()
	require.Contains(t, body, "上游拒绝了模型")
	require.Contains(t, body, "请核对账号编辑弹窗")
}
