package trae

import (
	"testing"
)

func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		region   Region
		want     bool
	}{
		{
			name:     "traecn explicit",
			raw:      map[string]any{"vendor": "traecn"},
			fileName: "any.json",
			region:   RegionCN,
			want:     true,
		},
		{
			name:     "traeglobal explicit",
			raw:      map[string]any{"vendor": "traeglobal"},
			fileName: "any.json",
			region:   RegionGlobal,
			want:     true,
		},
		{
			name:     "cross rejection",
			raw:      map[string]any{"vendor": "traeglobal"},
			fileName: "any.json",
			region:   RegionCN,
			want:     false,
		},
		{
			name:     "generic trae with global region",
			raw:      map[string]any{"region": "global"},
			fileName: "trae-acc.json",
			region:   RegionGlobal,
			want:     true,
		},
		{
			name:     "generic trae without region defaults to cn",
			raw:      map[string]any{},
			fileName: "trae-acc.json",
			region:   RegionCN,
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LooksLikeCredential(tc.raw, tc.fileName, "", tc.region)
			if got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestSoloLooksLikeCredential(t *testing.T) {
	solo := SoloConfig()
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		want     bool
	}{
		{"explicit vendor", map[string]any{"vendor": "traesolo", "access_token": "t"}, "any.json", true},
		{"filename prefix", map[string]any{"access_token": "t"}, "traesolo-u1.json", true},
		{"foreign vendor", map[string]any{"vendor": "workbuddycn"}, "traesolo-test.json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeCredentialFor(tc.raw, tc.fileName, "", solo); got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestSoloParseCredentialNested(t *testing.T) {
	raw := []byte(`{
		"auth": {"accessToken": "atk_123", "refreshToken": "rtk_456", "expiresAt": 1700000000},
		"account": {"uid": "user_789", "nickname": "Alice"}
	}`)
	cred, err := ParseCredentialFor(raw, SoloConfig())
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cred.AccessToken != "atk_123" || cred.RefreshToken != "rtk_456" || cred.UID != "user_789" {
		t.Fatalf("unexpected parsed result: %+v", cred)
	}
	if cred.Vendor != VendorIDSolo || cred.Region != RegionCN {
		t.Fatalf("vendor/region = %s/%s", cred.Vendor, cred.Region)
	}
}
