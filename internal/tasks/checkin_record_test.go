package tasks

import (
	"context"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// 签到落库的行为测试。
//
// 落库语义是「假性成功」的最后一道闸：只要签到结果被记成「今日已签到」，
// 控制台就会显示已签到，而上游窗口错过不补领——积分就此永久丢失。
// 因此「本次没真正领到」必须不落库。

// fakeDeps 是只实现落库所需能力的最小 Deps。
type fakeDeps struct {
	now   time.Time
	state state.State
}

func (f *fakeDeps) Registry() *core.Registry { return nil }
func (f *fakeDeps) Pool() *auth.Pool         { return nil }
func (f *fakeDeps) Config() Config           { return Config{} }
func (f *fakeDeps) State() state.State       { return f.state }
func (f *fakeDeps) Now() time.Time           { return f.now }

func (f *fakeDeps) MutateState(mutate func(*state.State)) {
	mutate(&f.state)
}

func (f *fakeDeps) RuntimeFields() []pluginapi.ConfigField { return nil }

func (f *fakeDeps) SaveRefreshedCredential(
	ctx context.Context, callbackID string, vendor core.Vendor,
	entry core.HostAuth, original []byte, updated *core.Credential, filePath string,
) {
}

// newFakeDeps 构造一个带好 map 的最小 Deps。
func newFakeDeps(now time.Time) *fakeDeps {
	return &fakeDeps{
		now:   now,
		state: state.State{Checkin: map[string]state.CheckinRecord{}},
	}
}

// TestRecordCheckinResultSkipsUnfinished 验证：未完成的签到不落库。
func TestRecordCheckinResultSkipsUnfinished(t *testing.T) {
	credential := &core.Credential{FileID: "qodercn-user"}
	deps := newFakeDeps(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	service := &Service{deps: deps}

	// 上游窗口未开启：本次什么都没领到。
	service.recordCheckinResult(credential, &core.CheckinResult{
		Message:     "上游领取窗口 10:00（UTC+8）开启，当前未开启",
		NotRecorded: true,
	})
	if len(deps.state.Checkin) != 0 {
		t.Fatalf("未完成的签到不应落库，实际 = %+v", deps.state.Checkin)
	}
}

// TestRecordCheckinResultRecordsCompleted 验证：真正领到的签到照常落库。
func TestRecordCheckinResultRecordsCompleted(t *testing.T) {
	credential := &core.Credential{FileID: "qodercn-user"}
	deps := newFakeDeps(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	service := &Service{deps: deps}

	service.recordCheckinResult(credential, &core.CheckinResult{Credit: 100})
	record, ok := deps.state.Checkin["qodercn-user"]
	if !ok {
		t.Fatal("已完成的签到应落库")
	}
	if record.LastDate != "2026-10-21" {
		t.Errorf("last_date = %q, 期望 2026-10-21", record.LastDate)
	}
	if record.Credit != 100 {
		t.Errorf("credit = %d, 期望 100", record.Credit)
	}
}

// TestRecordCheckinResultKeepsVendorDefault 验证：未声明 NotRecorded 的厂商
// 行为不变（零值兼容，其它厂商的签到记录照常写入）。
func TestRecordCheckinResultKeepsVendorDefault(t *testing.T) {
	credential := &core.Credential{FileID: "workbuddycn-user"}
	deps := newFakeDeps(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	service := &Service{deps: deps}

	// Already=true、Credit=0：既有厂商「已领取但本次无新积分」的形态。
	service.recordCheckinResult(credential, &core.CheckinResult{Already: true})
	record, ok := deps.state.Checkin["workbuddycn-user"]
	if !ok {
		t.Fatal("未声明 NotRecorded 的结果应照常落库（其它厂商行为不得改变）")
	}
	if record.LastDate != "2026-10-21" {
		t.Errorf("last_date = %q, 期望 2026-10-21", record.LastDate)
	}
}
