package zcode

import (
	"context"
	"errors"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
)

// 本文件实现 Start Plan 通道的按凭据串行化。
//
// 为什么需要它：Start Plan 是单用户交互套餐，上游对它的并发上限有明确表达
// （3008/3009/3010「并发上限」），高频并发还会触发 3012「unusual activity」
// 临时风控。官方客户端天然是串行的（一个用户在打字）；本插件把宿主转发来的
// 并发 agent 请求直接灌给上游，等价于把几十个会话塞进一个交互套餐——
// 上游按设计拒绝。串行化让本插件的出站行为与官方客户端一致：同一账号
// 同一时刻最多一个在途对话，后来者排队。
//
// 这不是绕过风控：上游以业务错误码明确声明了并发上限，客户端遵守自己
// 声明过的限制是标准做法；排队等不到时按 SoftRate 交还宿主正常换路。

const (
	// credentialQueueWaitTimeout 是排队等待在途对话出清的上限。
	// 覆盖常见流式回答的长度；超过后按 SoftRate 返回，由宿主决定换路或重试。
	credentialQueueWaitTimeout = 90 * time.Second
)

// errQueueTimeout 表示排队超过上限，未获得出站资格。
var errQueueTimeout = errors.New("zcode plan channel queue timeout")

// credentialGate 是单个凭据的出站闸门（容量 1 的信号量）。
type credentialGate struct {
	slot chan struct{}
}

var (
	gateMu sync.Mutex
	gates  = map[string]*credentialGate{}
)

// gateFor 返回该凭据的闸门（按 UserID 分，缺省共用一把）。
func gateFor(userID string) *credentialGate {
	gateMu.Lock()
	defer gateMu.Unlock()
	if userID == "" {
		userID = "-"
	}
	gate, ok := gates[userID]
	if !ok {
		gate = &credentialGate{slot: make(chan struct{}, 1)}
		gates[userID] = gate
	}
	return gate
}

// acquire 取得出站资格：无在途对话立即返回；否则有界排队。
// 请求 context 取消（宿主断开客户端）时提前退出，不占队列。
func (g *credentialGate) acquire(ctx context.Context) error {
	select {
	case g.slot <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(credentialQueueWaitTimeout)
	defer timer.Stop()
	select {
	case g.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errQueueTimeout
	}
}

// release 交还出站资格。仅应在 acquire 成功后调用一次。
func (g *credentialGate) release() {
	<-g.slot
}

// acquireGateForCredential 为本次对话取闸门；排队超时返回 SoftRate 类错误
// （不罚账号，宿主可换路重试）。取到后返回 release 函数。
func acquireGateForCredential(ctx context.Context, cred *Credential) (func(), error) {
	userID := ""
	if cred != nil {
		userID = cred.UserID
	}
	gate := gateFor(userID)
	if errAcquire := gate.acquire(ctx); errAcquire != nil {
		if errors.Is(errAcquire, errQueueTimeout) {
			return nil, &core.Failure{
				Kind:   core.FailureKindSoftRate,
				Status: 0,
				Msg:    "zcode Start Plan 通道并发排队超时（同账号在途对话未出清），已交还宿主换路",
			}
		}
		return nil, &core.Failure{Kind: core.FailureKindServer, Status: 0, Msg: "zcode chat cancelled while queued: " + errAcquire.Error()}
	}
	return gate.release, nil
}
