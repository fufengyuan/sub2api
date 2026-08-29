//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// composite 统一池的 8 个账号保险丝：只收紧、不放宽，且不影响其它分组口径。
func TestCapAccountAttemptsForGroup(t *testing.T) {
	composite := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	openai := &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}}

	// 配置默认 10 次切换（= 尝试 11 个账号）→ composite 收紧到 7 次切换（= 尝试 8 个）。
	require.Equal(t, compositeMaxAccountAttempts-1, capAccountAttemptsForGroup(composite, 10))
	// 配置本身更小时保持更小（只收紧不放宽）。
	require.Equal(t, 2, capAccountAttemptsForGroup(composite, 2))
	// 非 composite 分组不受影响。
	require.Equal(t, 10, capAccountAttemptsForGroup(openai, 10))
	require.Equal(t, 10, capAccountAttemptsForGroup(nil, 10))
	require.Equal(t, 3, capAccountAttemptsForGroup(&service.APIKey{}, 3))
}

// 软限流换号前退避，且单次请求内最多退避 maxDeferredSwitchBackoffs 次。
func TestFailoverStateAppliesSwitchBackoffWithBudget(t *testing.T) {
	fs := NewFailoverState(10, false)
	start := time.Now()
	action := fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 1, service.PlatformWorkBuddy, 0,
		&service.UpstreamFailoverError{StatusCode: http.StatusBadGateway, NextAccountRetryDelay: 40 * time.Millisecond})
	require.Equal(t, FailoverContinue, action)
	require.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond, "第一次换号应退避")
	require.Equal(t, 1, fs.deferredSwitchBackoffs)

	// 预算内的第二次仍退避。
	start = time.Now()
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 2, service.PlatformWorkBuddy, 0,
		&service.UpstreamFailoverError{StatusCode: http.StatusBadGateway, NextAccountRetryDelay: 40 * time.Millisecond}))
	require.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)

	// 超出预算后不再等待，但换号继续。
	start = time.Now()
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 3, service.PlatformWorkBuddy, 0,
		&service.UpstreamFailoverError{StatusCode: http.StatusBadGateway, NextAccountRetryDelay: time.Second}))
	require.Less(t, time.Since(start), 100*time.Millisecond, "退避预算耗尽后不得再等待")
	require.Equal(t, maxDeferredSwitchBackoffs, fs.deferredSwitchBackoffs)
	require.Equal(t, 3, fs.SwitchCount)
}

// 客户端断开时退避必须立刻中止，不再空等。
func TestFailoverStateSwitchBackoffRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fs := NewFailoverState(10, false)
	action := fs.HandleFailoverError(ctx, &mockTempUnscheduler{}, 1, service.PlatformWorkBuddy, 0,
		&service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, NextAccountRetryDelay: time.Minute})
	require.Equal(t, FailoverCanceled, action)
}
