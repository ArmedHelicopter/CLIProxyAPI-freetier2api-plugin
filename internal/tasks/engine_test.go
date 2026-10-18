package tasks

import "testing"

// TestBuildQueueItemsCollapsesAggregatedTasks 验证聚合任务（RunCode）每个账号只入队一次，
// 且队列里使用的是 RunCode 而不是明细任务码。
func TestBuildQueueItemsCollapsesAggregatedTasks(t *testing.T) {
	service := &Service{}
	scans := []accountScan{
		{
			UID: "u1", Label: "one",
			Tasks: []taskItem{
				{Kind: "growth", Code: "chat_5"},
				{Kind: "school", Code: "share_invite", RunCode: "school_daily"},
				{Kind: "school", Code: "chat_3_times", RunCode: "school_daily"},
			},
		},
		{
			UID: "u2", Label: "two",
			Tasks: []taskItem{
				{Kind: "school", Code: "expert_use", RunCode: "school_daily"},
			},
		},
	}

	items := service.buildQueueItems(scans, nil)
	if len(items) != 3 {
		t.Fatalf("buildQueueItems returned %d items, want 3: %#v", len(items), items)
	}
	want := map[string][]string{
		"u1": {"chat_5", "school_daily"},
		"u2": {"school_daily"},
	}
	for _, item := range items {
		found := false
		for _, code := range want[item.UID] {
			if item.Code == code {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected queue item for %s: %#v", item.UID, item)
		}
	}
}

// TestBuildQueueItemsFiltersByKind 验证旧控制台的 kind 筛选仍然生效。
func TestBuildQueueItemsFiltersByKind(t *testing.T) {
	service := &Service{}
	scans := []accountScan{{
		UID: "u1",
		Tasks: []taskItem{
			{Kind: "growth", Code: "chat_5"},
			{Kind: "school", Code: "share_invite", RunCode: "school_daily"},
		},
	}}
	items := service.buildQueueItems(scans, map[string]bool{"school": true})
	if len(items) != 1 || items[0].Code != "school_daily" {
		t.Fatalf("filtered queue = %#v, want only school_daily", items)
	}
}
