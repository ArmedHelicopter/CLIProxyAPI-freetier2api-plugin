package app

import "testing"

func TestDefaultVersionMatchesForkRelease(t *testing.T) {
	if DefaultVersion != "0.1.20" {
		t.Fatalf("DefaultVersion = %q, want 0.1.20", DefaultVersion)
	}
	instance, errNew := New(Options{})
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}
	if instance.Version() != DefaultVersion {
		t.Fatalf("instance version = %q, want %q", instance.Version(), DefaultVersion)
	}
}
