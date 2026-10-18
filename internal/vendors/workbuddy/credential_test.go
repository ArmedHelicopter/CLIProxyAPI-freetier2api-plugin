package workbuddy

import (
	"encoding/json"
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestParseCredentialBothShapes 验证凭证的两种形态都能解析。
func TestParseCredentialBothShapes(t *testing.T) {
	nested := `{"auth":{"accessToken":"at","refreshToken":"rt","domain":"www.codebuddy.cn"},
	             "account":{"uid":"u1","nickname":"昵称"},"device_token":"dt"}`
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u1","domain":"www.workbuddy.ai","device_token":"dt"}`

	for _, testCase := range []struct {
		name       string
		raw        string
		wantRealm  Region
		wantUID    string
		wantDevice string
	}{
		{"nested", nested, RegionCN, "u1", "dt"},
		// 扁平形态按 domain 推断域：workbuddy.ai 家族是国际版。
		{"flat global", flat, RegionGlobal, "u1", "dt"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			credential, errParse := ParseCredential([]byte(testCase.raw), RegionCN)
			if errParse != nil {
				t.Fatalf("ParseCredential: %v", errParse)
			}
			if credential.Realm() != testCase.wantRealm {
				t.Fatalf("realm = %s, want %s", credential.Realm(), testCase.wantRealm)
			}
			if credential.UIDValue() != testCase.wantUID {
				t.Fatalf("uid = %q, want %q", credential.UIDValue(), testCase.wantUID)
			}
			if credential.DeviceTokenValue() != testCase.wantDevice {
				t.Fatalf("device_token = %q", credential.DeviceTokenValue())
			}
		})
	}
}

// TestParseCredentialRejectsEmpty 验证空凭证被拒绝。
func TestParseCredentialRejectsEmpty(t *testing.T) {
	if _, errParse := ParseCredential([]byte(`{}`), RegionCN); errParse == nil {
		t.Fatal("credential without any token must be rejected")
	}
	if _, errParse := ParseCredential(nil, RegionCN); errParse == nil {
		t.Fatal("empty credential must be rejected")
	}
}

// TestMergeStorageJSONKeepsUserFields 验证写回时保留用户维护的字段。
//
// 宿主 auth 文件里可能有 disabled / prefix / proxy_url / note / weight 等
// 用户自己加的字段；重建式写回会静默丢掉它们。
func TestMergeStorageJSONKeepsUserFields(t *testing.T) {
	original := []byte(`{"auth":{"accessToken":"old"},"disabled":true,"note":"我的备注","weight":3}`)
	credential := &Credential{AccessToken: "new", RefreshToken: "rt", UID: "u1"}
	credential.SetRealm(RegionCN)

	merged, errMerge := MergeStorageJSON(original, credential)
	if errMerge != nil {
		t.Fatalf("MergeStorageJSON: %v", errMerge)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &payload); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if payload["disabled"] != true {
		t.Fatal("user field 'disabled' must be preserved")
	}
	if payload["note"] != "我的备注" {
		t.Fatal("user field 'note' must be preserved")
	}
	auth, okAuth := payload["auth"].(map[string]any)
	if !okAuth || auth["accessToken"] != "new" {
		t.Fatalf("token must be updated: %v", payload["auth"])
	}
}

// TestStorageJSONAlwaysCarriesType 验证写回的凭证一定带 type 字段。
//
// 宿主用 type 直接判定归属（有它就不再遍历插件）。缺这个字段时归属取决于
// 插件遍历顺序，会被判定过宽的前置插件抢走，账号在宿主侧被归到别的 provider。
func TestStorageJSONAlwaysCarriesType(t *testing.T) {
	credential := &Credential{AccessToken: "at", RefreshToken: "rt", UID: "u1"}
	credential.SetRealm(RegionCN)

	raw, errStorage := credential.StorageJSON()
	if errStorage != nil {
		t.Fatalf("StorageJSON: %v", errStorage)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if payload["type"] != ProviderKey {
		t.Fatalf("type = %v, want %q", payload["type"], ProviderKey)
	}

	// 合并写回：原文件缺 type 时也要补上（用户从别处复制进来的旧凭证）。
	merged, errMerge := MergeStorageJSON([]byte(`{"auth":{"accessToken":"old"},"uid":"u1"}`), credential)
	if errMerge != nil {
		t.Fatalf("MergeStorageJSON: %v", errMerge)
	}
	var mergedPayload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &mergedPayload); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if mergedPayload["type"] != ProviderKey {
		t.Fatalf("merged type = %v, want %q", mergedPayload["type"], ProviderKey)
	}
	// 用户字段仍要保留。
	if mergedPayload["uid"] != "u1" {
		t.Fatal("merge must preserve user fields")
	}
}

// TestLooksLikeCredential 验证凭证归属判定。
//
// 判定必须偏严：宿主把所有非内建格式的文件依次喂给每个插件，
// 误吞别家的凭证会让宿主用本插件结构改写对方的账号文件（且是静默的）。
func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		provider string
		want     bool
	}{
		// 强证据：显式声明。
		{"host provider hint", map[string]any{}, "x.json", ProviderKey, true},
		{"type field", map[string]any{"type": ProviderKey}, "x.json", "", true},
		{"type field uppercase", map[string]any{"type": "FreeTier"}, "x.json", "", true},

		// 文件名约定。CN 与 GLOBAL 是独立供应商，两者都要被本协议认出
		// （具体区域由 core.ResolveVendor 的前缀匹配决定）。
		{"filename exact", map[string]any{"foo": "bar"}, "workbuddy.json", "", true},
		{"filename cn vendor", map[string]any{"foo": "bar"}, "workbuddycn-abc.json", "", true},
		{"filename global vendor", map[string]any{"foo": "bar"}, "workbuddyglobal-abc.json", "", true},
		{"filename underscore suffix", map[string]any{"foo": "bar"}, "workbuddy_abc.json", "", true},
		{"filename contains segment", map[string]any{"foo": "bar"}, "my-workbuddy.json", "", true},

		// 本插件独有的嵌套结构。
		{"nested auth pair", map[string]any{
			"auth": map[string]any{"accessToken": "a", "refreshToken": "r"},
		}, "x.json", "", true},

		// **必须拒绝**：通用字段不足以判定归属。
		{"top-level accessToken only", map[string]any{"accessToken": "a"}, "x.json", "", false},
		{"top-level both tokens", map[string]any{"accessToken": "a", "refreshToken": "r"}, "x.json", "", false},
		{"device_token only", map[string]any{"device_token": "d"}, "x.json", "", false},
		{"nested accessToken without refresh", map[string]any{
			"auth": map[string]any{"accessToken": "a"},
		}, "x.json", "", false},

		// 明确的别家 provider。
		{"foreign type", map[string]any{"type": "qoder"}, "x.json", "qoder", false},
		{"foreign filename", map[string]any{"access_token": "x"}, "claude.json", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := LooksLikeCredential(testCase.raw, testCase.fileName, testCase.provider); got != testCase.want {
				t.Fatalf("LooksLikeCredential = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestParseCredentialStillAcceptsFlatShape 验证收紧归属判定后，
// 扁平形态（手写凭证）仍能被解析——判定严不等于解析窄。
func TestParseCredentialStillAcceptsFlatShape(t *testing.T) {
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u1","domain":"www.codebuddy.cn"}`
	credential, errParse := ParseCredential([]byte(flat), RegionCN)
	if errParse != nil {
		t.Fatalf("flat credential must still parse: %v", errParse)
	}
	if credential.UIDValue() != "u1" || credential.AccessTokenValue() != "at" {
		t.Fatalf("flat credential parsed incorrectly: %+v", credential)
	}
	// 但归属判定不因它能解析就放行（除非文件名符合约定）。
	if LooksLikeCredential(map[string]any{"accessToken": "at", "refreshToken": "rt"}, "mystery.json", "") {
		t.Fatal("a parseable flat credential from an unknown file must not be claimed")
	}
}

// TestDesktopChatSequenceShape 验证对话链的结构与关键字段。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	if len(events) != 6 {
		t.Fatalf("chat sequence must have 6 events, got %d", len(events))
	}

	codes := make([]string, 0, len(events))
	for _, event := range events {
		code, _ := event["eventCode"].(string)
		codes = append(codes, code)
	}
	want := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	for index, expected := range want {
		if codes[index] != expected {
			t.Fatalf("event %d = %q, want %q", index, codes[index], expected)
		}
	}

	// chat_message_response 必须带成功回执：服务端对事件链有真实性校验，
	// 缺了它整条链不会计分。
	response := events[3]
	if response["isSuccessful"] != true {
		t.Fatal("chat_message_response must carry isSuccessful=true")
	}
	// 会话标识必须贯穿。
	if events[2]["codebuddy.session_id"] != "conv-1" {
		t.Fatalf("session id missing on chat_request_send: %v", events[2]["codebuddy.session_id"])
	}
	if events[2]["codebuddy.conversation_request_id"] != "req-1" {
		t.Fatal("conversation request id must be the round-aggregation key")
	}
}

// TestDesktopBuddyAppSequence 验证 Buddy 应用链（同时满足两个任务）。
func TestDesktopBuddyAppSequence(t *testing.T) {
	events := DesktopBuddyAppSequence(DefaultBuddyID, DefaultBuddyName)
	if len(events) != 5 {
		t.Fatalf("buddy app sequence must have 5 events, got %d", len(events))
	}
	for index, event := range events {
		if event["buddyId"] != DefaultBuddyID {
			t.Fatalf("event %d must carry the buddy id", index)
		}
		if event["mode"] != "LOCAL" {
			t.Fatalf("event %d mode = %v, want LOCAL", index, event["mode"])
		}
	}
}

// TestDesktopTemplateUseSequence 验证模板链 = 对话链 + 2 个模板事件。
func TestDesktopTemplateUseSequence(t *testing.T) {
	events := DesktopTemplateUseSequence("conv", "req", "tpl-1", "深度研究")
	if len(events) != 8 {
		t.Fatalf("template sequence = chat(6) + 2, got %d", len(events))
	}
	if events[6]["eventCode"] != "agent_task_created_with_template" {
		t.Fatalf("event 6 = %v", events[6]["eventCode"])
	}
	if events[7]["eventCode"] != "template_used" {
		t.Fatalf("event 7 = %v", events[7]["eventCode"])
	}
	// 模板 id 会带到事件里（服务端不校验真实性，但不能为空）。
	if events[7]["template_id"] != "tpl-1" {
		t.Fatal("template id must be carried")
	}
}

// TestDesktopPlaybookPromptSequence 验证灵感案例链的判据事件。
func TestDesktopPlaybookPromptSequence(t *testing.T) {
	events := DesktopPlaybookPromptSequence("conv", "req", "case-1", "案例名")
	if len(events) != 9 {
		t.Fatalf("playbook sequence = chat(6) + 3, got %d", len(events))
	}
	// 判据是 playbook_prompt_send（真正的发送动作），不是卡片点击。
	if events[8]["eventCode"] != "playbook_prompt_send" {
		t.Fatalf("last event = %v, want playbook_prompt_send", events[8]["eventCode"])
	}
	if events[8]["conversationId"] != "conv" || events[8]["requestId"] != "req" {
		t.Fatal("prompt send must carry conversation and request ids")
	}
}

// TestDesktopDesignCanvasSequence 验证设计画布链。
func TestDesktopDesignCanvasSequence(t *testing.T) {
	events := DesktopDesignCanvasSequence("conv", "abcdefgh12345678")
	if len(events) != 8 {
		t.Fatalf("canvas sequence = chat(6) + 2, got %d", len(events))
	}
	if events[6]["eventCode"] != "wbx_design_canvas_task_create" {
		t.Fatalf("event 6 = %v", events[6]["eventCode"])
	}
	if events[7]["eventCode"] != "wbx_design_canvas_open" {
		t.Fatalf("event 7 = %v", events[7]["eventCode"])
	}
	// 画布 id 由 requestId 尾部派生。
	if events[7]["id"] != "ardot-file-12345678" {
		t.Fatalf("canvas id = %v, want ardot-file-<last8>", events[7]["id"])
	}
}

// TestSchoolChatTimesEventsActivityID 验证开学季的 activityId 门控。
//
// 校园日任务要求事件带 activityId，缺失则不点亮。
func TestSchoolChatTimesEventsActivityID(t *testing.T) {
	withActivity := SchoolChatTimesEvents("conv", SchoolSeasonActivityID)
	if len(withActivity) != 1 {
		t.Fatalf("expected 1 event, got %d", len(withActivity))
	}
	if withActivity[0]["activityId"] != SchoolSeasonActivityID {
		t.Fatal("activity id must be present when requested")
	}

	withoutActivity := SchoolChatTimesEvents("conv", "")
	if _, okActivity := withoutActivity[0]["activityId"]; okActivity {
		t.Fatal("activity id must be absent when not requested")
	}
	// 小程序事件用 mp 身份。
	if withoutActivity[0]["agentName"] != "mp" {
		t.Fatalf("agentName = %v, want mp", withoutActivity[0]["agentName"])
	}
}

// TestMiniChatModelEvent 验证小程序指定模型事件。
func TestMiniChatModelEvent(t *testing.T) {
	events := MiniChatModelEvent("conv", "glm-5.2", "GLM-5.2")
	if events[0]["requestModelId"] != "glm-5.2" || events[0]["requestModelName"] != "GLM-5.2" {
		t.Fatalf("model fields missing: %+v", events[0])
	}
}

// TestMiniExpertUseEventShape 验证小程序专家事件的字段形态。
//
// 真实事件**不带** conversationId / activityId，多带字段反而可能不被识别。
func TestMiniExpertUseEventShape(t *testing.T) {
	events := MiniExpertUseEvent("ex_realid", "专家名", "agent")
	event := events[0]
	if event["eventCode"] != "expert_actual_use" {
		t.Fatalf("eventCode = %v", event["eventCode"])
	}
	if event["type"] != "send_message" {
		t.Fatalf("mp expert event type must be send_message, got %v", event["type"])
	}
	if event["source"] != "mini_program" {
		t.Fatalf("source = %v, want mini_program", event["source"])
	}
	for _, absent := range []string{"conversationId", "activityId"} {
		if _, okField := event[absent]; okField {
			t.Fatalf("%s must be absent on mp expert events", absent)
		}
	}
}

// TestApplyDesktopFingerprintInjectsCommonFields 验证指纹注入。
func TestApplyDesktopFingerprintInjectsCommonFields(t *testing.T) {
	credential := testTaskCredential("uid-1")
	rt := newRuntime(core.Env{})
	events := ApplyDesktopFingerprint(credential, []map[string]any{
		{"eventCode": "x", "userId": "business-wins"},
	}, rt)
	event := events[0]
	// 业务字段优先于指纹基底。
	if event["userId"] != "business-wins" {
		t.Fatalf("business field must win over the fingerprint base, got %v", event["userId"])
	}
	// 指纹字段补齐。
	for _, key := range []string{"machineId", "sessionId", "extName", "ideName", "timezone"} {
		if _, okField := event[key]; !okField {
			t.Fatalf("fingerprint field %q must be injected", key)
		}
	}
	if event["extName"] != "workbuddy-desktop" {
		t.Fatalf("extName = %v (this is what routes the event to the desktop task family)", event["extName"])
	}
}

// TestFingerprintStability 验证设备指纹的稳定性与隔离性。
func TestFingerprintStability(t *testing.T) {
	credentialA := testTaskCredential("uid-a")
	credentialB := testTaskCredential("uid-b")

	rt := newRuntime(core.Env{})
	machineA := rt.fingerprint(credentialA, "machine")
	if machineA != rt.fingerprint(credentialA, "machine") {
		t.Fatal("fingerprint must be stable")
	}
	if len(machineA) != 36 {
		t.Fatalf("fingerprint length = %d, want 36", len(machineA))
	}
	if rt.fingerprint(credentialB, "machine") == machineA {
		t.Fatal("different accounts must get different fingerprints")
	}
	if rt.fingerprint(credentialA, "session") == machineA {
		t.Fatal("different purposes must get different fingerprints")
	}
}

// TestRandomHex 验证随机 ID 生成。
func TestRandomHex(t *testing.T) {
	first := randomHex(8)
	second := randomHex(8)
	if len(first) != 16 {
		t.Fatalf("randomHex(8) length = %d, want 16", len(first))
	}
	if first == second {
		t.Fatal("randomHex must not repeat")
	}
}
