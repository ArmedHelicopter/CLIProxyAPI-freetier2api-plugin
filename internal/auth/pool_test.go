package auth

import (
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestTargetMatchesEntryNormalizesCredentialIdentity(t *testing.T) {
	entry := core.HostAuth{
		ID:        "zcode-eyjh---1kc8.json",
		Name:      "zcode-eyJh---1KC8.json",
		AuthIndex: "9ec039baa3689edd",
	}
	for _, target := range []string{
		"zcode-eyjh---1kc8",
		"ZCODE-EYJH---1KC8.JSON",
		"9EC039BAA3689EDD",
	} {
		if !targetMatchesEntry(target, entry) {
			t.Errorf("targetMatchesEntry(%q, entry) = false, want true", target)
		}
	}
}
