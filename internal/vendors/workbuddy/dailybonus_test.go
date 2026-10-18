package workbuddy

// 本文件锁定每日活跃奖励（国际版签到）的关键判据。
//
// 信标与热力图解析都是「错了不报错、只是静默不入账」的链路：
// 事件字段名写错上游返回 200 但丢事件，热力图形态解析错会把未点亮
// 误判成已点亮。因此用单测把字段名与两种响应形态锁住。

import (
	"encoding/json"
	"testing"
)

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
