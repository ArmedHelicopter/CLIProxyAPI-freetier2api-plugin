package zcode

import (
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestActivePlansExcludesFuturePlans(t *testing.T) {
	plans := []map[string]any{
		{"plan_id": "future", "status": "active", "starts_at": float64(2000), "ends_at": float64(3000)},
		{"plan_id": "current", "status": "active", "starts_at": float64(1000), "ends_at": float64(3000)},
	}
	active := activePlans(plans, 1500)
	if len(active) != 1 || core.RawStringField(active[0], "plan_id") != "current" {
		t.Fatalf("active plans = %+v", active)
	}
}
