package workbuddy

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件锁定每日活跃奖励（国际版签到）的关键判据。
//
// 信标与热力图解析都是「错了不报错、只是静默不入账」的链路：
// 事件字段名写错上游返回 200 但丢事件，热力图形态解析错会把未点亮
// 误判成已点亮。因此用单测把字段名与两种响应形态锁住。

func testGlobalCredential(uid string) *Credential {
	credential := &Credential{
		AccessToken:  "at",
		RefreshToken: "rt",
		UID:          uid,
		Nickname:     "global tester",
		Domain:       "www.workbuddy.ai",
	}
	credential.SetRealm(RegionGlobal)
	return credential
}

// TestDailyBonusBeaconEventShape 锁定信标事件的字段形态。
func TestDailyBonusBeaconEventShape(t *testing.T) {
	event := DailyBonusBeaconEvent(testGlobalCredential("uid-1234"))

	if event["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode = %v, want chat_request_send", event["eventCode"])
	}
	// userId 缺失时上游 200 但静默丢弃事件。
	if event["userId"] != "uid-1234" {
		t.Fatalf("userId = %v, want uid-1234", event["userId"])
	}
	// 会话/请求 id 必须同值，且每天应像新会话（非固定重放值）。
	conversation, _ := event["conversationId"].(string)
	request, _ := event["requestId"].(string)
	root, _ := event["rootRequestId"].(string)
	parent, _ := event["parentConversationId"].(string)
	if conversation == "" || conversation != request || request != root || root != parent {
		t.Fatalf("conversation/request ids must be non-empty and equal: conv=%q req=%q root=%q parent=%q",
			conversation, request, root, parent)
	}
	again := DailyBonusBeaconEvent(testGlobalCredential("uid-1234"))
	againConversation, _ := again["conversationId"].(string)
	if againConversation == conversation {
		t.Fatal("each beacon must mint a fresh conversation id (day looks like a new conversation)")
	}
	// 零值切片字段必须存在（上游要求字段在，哪怕为空）。
	for _, key := range []string{"mentionContexts", "knowledgeId", "knowledgeName"} {
		if _, ok := event[key].([]any); !ok {
			t.Fatalf("%s must be present as an array, got %T", key, event[key])
		}
	}
	if event["agentName"] != "default" || event["agentType"] != "conversation" {
		t.Fatalf("agent fields mismatch: %v / %v", event["agentName"], event["agentType"])
	}
	if event["requestModelId"] != "deepseek-v4-flash" {
		t.Fatalf("requestModelId = %v", event["requestModelId"])
	}
}

// TestParseGrowthHeatmapCells 锁定 cells 数组形态。
func TestParseGrowthHeatmapCells(t *testing.T) {
	data := json.RawMessage(`{"cells":[
		{"date":"2026-10-16","score":0},
		{"date":"2026-10-17","score":30},
		{"date":"2026-10-18","score":0}
	]}`)
	today, lit, err := parseGrowthHeatmap(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if today.Date != "2026-10-18" || today.Score != 0 {
		t.Fatalf("today = %+v, want last cell 2026-10-18/0", today)
	}
	if len(lit) != 1 || lit[0] != "2026-10-17" {
		t.Fatalf("lit = %v, want [2026-10-17]", lit)
	}
}

// TestParseGrowthHeatmapByDate 锁定日期为键的 map 形态。
func TestParseGrowthHeatmapByDate(t *testing.T) {
	data := json.RawMessage(`{"2026-10-16":{"score":0},"2026-10-18":{"score":5},"2026-10-17":{"score":30}}`)
	today, lit, err := parseGrowthHeatmap(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if today.Date != "2026-10-18" || today.Score != 5 {
		t.Fatalf("today = %+v, want 2026-10-18/5", today)
	}
	if len(lit) != 2 || lit[0] != "2026-10-17" || lit[1] != "2026-10-18" {
		t.Fatalf("lit = %v, want sorted [2026-10-17 2026-10-18]", lit)
	}
}

// TestParseGrowthHeatmapEmpty 空热力图必须报错，不能误判成「未点亮」。
func TestParseGrowthHeatmapEmpty(t *testing.T) {
	if _, _, err := parseGrowthHeatmap(json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty heatmap must error")
	}
	if _, _, err := parseGrowthHeatmap(json.RawMessage(`{"cells":[]}`)); err == nil {
		t.Fatal("empty cells must error")
	}
}

// TestParseClaimResultRejectsMalformedPayloads 领奖响应解析失败必须显式报错，
// 不能把 malformed JSON/HTML 静默当成 0 奖励成功。
func TestParseClaimResultRejectsMalformedPayloads(t *testing.T) {
	for _, raw := range []string{
		`{`,
		`<html><body>upstream error</body></html>`,
		`{"unexpected":true}`,
		`null`,
	} {
		t.Run(raw, func(t *testing.T) {
			credit, energy, errParse := parseClaimResult(json.RawMessage(raw))
			if errParse == nil {
				t.Fatalf("parseClaimResult(%q) returned nil error with %d/%d reward", raw, credit, energy)
			}
			if credit != 0 || energy != 0 {
				t.Fatalf("malformed claim result returned reward %d/%d", credit, energy)
			}
		})
	}
}

func TestParseClaimResult(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		raw          string
		wantCredit   int64
		wantEnergy   int64
		wantNilError bool
	}{
		{"reward", `{"credit":12,"energy":3}`, 12, 3, true},
		{"already claimed", `{"already_claimed":true}`, 0, 0, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			credit, energy, errParse := parseClaimResult(json.RawMessage(testCase.raw))
			if (errParse == nil) != testCase.wantNilError {
				t.Fatalf("error = %v, want nil=%v", errParse, testCase.wantNilError)
			}
			if credit != testCase.wantCredit || energy != testCase.wantEnergy {
				t.Fatalf("reward = %d/%d, want %d/%d", credit, energy, testCase.wantCredit, testCase.wantEnergy)
			}
		})
	}
}

// TestTaskSpecsForGlobalRegistersDailyBonus 国际版必须注册每日活跃奖励族。
func TestTaskSpecsForGlobalRegistersDailyBonus(t *testing.T) {
	specs := TaskSpecsFor(RegionGlobal)
	if len(specs) != 1 {
		t.Fatalf("global specs = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Kind != TaskKindDailyBonus || spec.Route != "/dailybonus" || !spec.Manual {
		t.Fatalf("spec = %+v", spec)
	}
	if len(spec.DefaultHours) != len(GlobalDailyBonusHours) {
		t.Fatalf("default hours = %v, want %v", spec.DefaultHours, GlobalDailyBonusHours)
	}
	// CN 不注册该族（CN 走 checkin 族）。
	for _, cnSpec := range TaskSpecsFor(RegionCN) {
		if cnSpec.Kind == TaskKindDailyBonus {
			t.Fatal("CN must not register the daily bonus family")
		}
	}
}

// testTaskCredential 构造一个测试凭证。
func testTaskCredential(uid string) *Credential {
	credential := &Credential{
		AccessToken:  "at",
		RefreshToken: "rt",
		UID:          uid,
		Nickname:     "测试账号",
		Domain:       "www.codebuddy.cn",
	}
	credential.SetRealm(RegionCN)
	return credential
}

// TestInNightWindow 验证夜猫子窗口判定。
//
// 窗口外跑夜猫子任务既消耗额度又不计分，因此边界必须准确。
func TestInNightWindow(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{0, true}, {7, true}, {8, false}, {12, false},
		{22, false}, {23, true},
	}
	for _, testCase := range cases {
		now := time.Date(2026, 9, 26, testCase.hour, 30, 0, 0, time.Local)
		if got := InNightWindow(now); got != testCase.want {
			t.Fatalf("InNightWindow(%02d:30) = %v, want %v", testCase.hour, got, testCase.want)
		}
	}
}

// TestParseBalanceMatchesUpstreamShape 验证余额解析对齐上游的真实契约。
//
// 上游该接口用 PascalCase 字段、双层信封（Response.Data.Accounts），
// 且 Cycle 期套餐优先。早期实现按 camelCase 解析，结果恒为 0。
func TestParseBalanceMatchesUpstreamShape(t *testing.T) {
	raw := `{"Response":{"Data":{"Accounts":[
	  {"PackageName":"基础套餐","CycleCapacitySize":1000,"CycleCapacityRemain":400,"CycleCapacityUsed":600},
	  {"PackageName":"加油包","CapacitySize":500,"CapacityRemain":500}
	]}}}`
	var payload billingResourceResponse
	if errUnmarshal := json.Unmarshal([]byte(raw), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	balance := parseBalance(payload)

	// Cycle 套餐：1000 总量、400 剩余；加油包：500/500。合计 1500 / 900。
	if balance.Total != 1500 {
		t.Fatalf("total = %d, want 1500", balance.Total)
	}
	if balance.Remain != 900 {
		t.Fatalf("remain = %d, want 900", balance.Remain)
	}
	if len(balance.Packages) != 2 {
		t.Fatalf("packages = %d, want 2", len(balance.Packages))
	}
	if balance.Packages[0].Name != "基础套餐" || balance.Packages[0].Remain != 400 {
		t.Fatalf("first package = %+v", balance.Packages[0])
	}
}

// TestPackageRemainUsedCycleWins 验证 Cycle 期套餐的取数规则。
func TestPackageRemainUsedCycleWins(t *testing.T) {
	// CycleUsed 大于 size-remain 时，应以 CycleUsed 为准反推 remain。
	remain, used, size := packageRemainUsed(billingResourceAccount{
		CycleCapacitySize: 1000, CycleCapacityRemain: 900, CycleCapacityUsed: 300,
	})
	if size != 1000 || used != 300 || remain != 700 {
		t.Fatalf("cycle rule: remain=%d used=%d size=%d, want 700/300/1000", remain, used, size)
	}
	// 无 Cycle 期时回落 Capacity 三字段。
	remain, used, size = packageRemainUsed(billingResourceAccount{
		CapacitySize: 500, CapacityRemain: 200,
	})
	if size != 500 || used != 300 || remain != 200 {
		t.Fatalf("capacity fallback: remain=%d used=%d size=%d, want 200/300/500", remain, used, size)
	}
	// remain 超出 size 时钳到 size（上游偶发脏数据）。
	remain, _, size = packageRemainUsed(billingResourceAccount{
		CycleCapacitySize: 100, CycleCapacityRemain: 999,
	})
	if remain != 100 || size != 100 {
		t.Fatalf("clamp: remain=%d size=%d, want 100/100", remain, size)
	}
}
