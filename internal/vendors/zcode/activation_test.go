package zcode

import (
	"context"
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

// 活跃上报的纯函数测试（不触网）。
//
// 覆盖三件会静默出错、且出错表现都是「活动列表恒空」的事：
//   - 事件体字段集必须与官方 sendReport 一致（少字段上游可能静默忽略）；
//   - 每账号每日只报一次（重复上报没有收益，只会多打上游）；
//   - 日期按账号档案时区算（官方 reportAppDailyActive 的 toLocalDateKey 同义）。

func TestActivationEventBodyMatchesOfficialFields(t *testing.T) {
	cred := &Credential{
		JWTToken: "jwt",
		Profile: DeviceProfile{
			Platform: "win32", Arch: "x64", OSVersion: "10.0.22631",
			Language: "zh-CN", Timezone: "Asia/Shanghai",
			Screen: "1920x1080", DeviceMID: "mid-1",
		},
	}
	body := activationEventBody("app_launch", cred, "user-1")

	// 官方 sendReport 的 16 个键，一个都不能少（多一个不会被拒，少一个可能被忽略）。
	want := []string{
		"event_id", "client_timezone", "client_language", "element_name",
		"event_region", "event_type", "event_text", "event_extra_detail",
		"user_id", "screen_resolution", "app_version", "device_os_category",
		"device_os_version", "device_mid", "mac_id", "marketing_params",
	}
	for _, key := range want {
		if _, ok := body[key]; !ok {
			t.Fatalf("事件体缺少字段 %q: %+v", key, body)
		}
	}
	if len(body) != len(want) {
		t.Fatalf("事件体字段数 = %d, want %d: %+v", len(body), len(want), body)
	}

	if body["element_name"] != "app_launch" || body["event_region"] != "app" || body["event_type"] != "view" {
		t.Fatalf("事件语义字段错误: %+v", body)
	}
	// 设备字段必须取自账号档案，否则多账号会聚成同一台机器。
	if body["device_mid"] != "mid-1" || body["device_os_category"] != "windows" ||
		body["screen_resolution"] != "1920x1080" || body["client_timezone"] != "Asia/Shanghai" {
		t.Fatalf("设备字段未取自账号档案: %+v", body)
	}
	if body["marketing_params"] != "{}" || body["mac_id"] != "" {
		t.Fatalf("固定字段错误: %+v", body)
	}
	if body["event_id"] == "" || body["event_id"] == activationEventBody("app_launch", cred, "user-1")["event_id"] {
		t.Fatal("event_id 每次必须新生成")
	}
}

func TestActivationElementsAreOfficialPair(t *testing.T) {
	// 两条缺一不可：app_launch 是启动，app_daily_active 是日活。
	want := []string{"app_launch", "app_daily_active"}
	if len(ActivationElements) != len(want) {
		t.Fatalf("ActivationElements = %v", ActivationElements)
	}
	for index, element := range want {
		if ActivationElements[index] != element {
			t.Fatalf("ActivationElements[%d] = %q, want %q", index, ActivationElements[index], element)
		}
	}
}

func TestActivationStateDedupesPerUserPerDay(t *testing.T) {
	state := newActivationState()
	if !state.shouldReport("u1", "2026-10-23") {
		t.Fatal("首次上报应放行")
	}
	if state.shouldReport("u1", "2026-10-23") {
		t.Fatal("同一账号同一天不应重复上报")
	}
	// 账号之间互不影响。
	if !state.shouldReport("u2", "2026-10-23") {
		t.Fatal("不同账号应各自上报")
	}
	// 跨日重新放行。
	if !state.shouldReport("u1", "2026-10-24") {
		t.Fatal("跨日应重新上报")
	}
}

func TestActivationStateForgetsOnFailure(t *testing.T) {
	state := newActivationState()
	state.shouldReport("u1", "2026-10-23")
	// 上报失败必须撤销占位，否则整个自然日都拿不到活动。
	state.forget("u1")
	if !state.shouldReport("u1", "2026-10-23") {
		t.Fatal("失败后应允许重试")
	}
}

func TestLocalDateFollowsProfileTimezone(t *testing.T) {
	// 2026-10-23T20:00:00Z：上海已是 10-24，纽约仍是 10-23。
	at := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	if got := localDate(DeviceProfile{Timezone: "Asia/Shanghai"}, at); got != "2026-10-24" {
		t.Fatalf("上海日期 = %q, want 2026-10-24", got)
	}
	if got := localDate(DeviceProfile{Timezone: "America/New_York"}, at); got != "2026-10-23" {
		t.Fatalf("纽约日期 = %q, want 2026-10-23", got)
	}
	// 时区缺失/非法时退回 UTC，而不是 panic 或空串。
	if got := localDate(DeviceProfile{}, at); got != "2026-10-23" {
		t.Fatalf("无时区日期 = %q, want 2026-10-23", got)
	}
	if got := localDate(DeviceProfile{Timezone: "Not/AZone"}, at); got != "2026-10-23" {
		t.Fatalf("非法时区日期 = %q, want 2026-10-23", got)
	}
}

func TestEnsureActivationExplainsMissingUserID(t *testing.T) {
	// 无 user_id 时不上报，但必须给出原因：否则用户只会看到永远空的活动列表。
	v := Instances(core.Env{})[0].(*adapter)
	reason := v.ensureActivation(context.Background(), &Credential{JWTToken: "not-a-jwt"})
	if reason == "" {
		t.Fatal("缺少 user_id 时必须返回原因")
	}
	if reason := v.ensureActivation(context.Background(), nil); reason != "" {
		t.Fatalf("nil 凭证应静默, got %q", reason)
	}
}
