package admin

import (
	"strings"

	"freetier2api-plugin/internal/core"
)

// vendorDescriptor 是供应商的展示描述（供页面下拉与分组用）。
type vendorDescriptor struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Region          string `json:"region"`
	SupportsCheckin bool   `json:"supports_checkin"`
	AuthMode        string `json:"auth_mode"`
	SupportsLogin   bool   `json:"supports_login"`
}

func (s *Service) buildVendorsPayload() map[string]any {
	cfg := s.deps.Config()
	vendors := make([]vendorDescriptor, 0, len(s.registry.Vendors()))
	for _, vendor := range s.registry.Vendors() {
		if !realmEnabled(cfg, vendor.Region()) {
			continue
		}
		vendors = append(vendors, vendorDescriptor{
			ID:              vendor.ID(),
			Name:            vendor.Name(),
			Region:          vendor.Region(),
			SupportsCheckin: core.SupportsCheckin(vendor),
			AuthMode:        vendorAuthMode(vendor),
			SupportsLogin:   vendorSupportsLogin(vendor),
		})
	}
	return map[string]any{"ok": true, "vendors": vendors}
}

func vendorAuthMode(vendor core.Vendor) string {
	if reporter, okReporter := vendor.(core.AuthModeReporter); okReporter {
		if mode := strings.TrimSpace(reporter.AuthMode()); mode != "" {
			return mode
		}
	}
	return "oauth"
}

func vendorSupportsLogin(vendor core.Vendor) bool {
	_, okLogin := core.As[core.LoginProvider](vendor)
	return okLogin
}
