package loginsession

import (
	"sync"
	"testing"
	"time"
)

func TestRegisterPollOwnsDelete(t *testing.T) {
	Register("vendor-a", "sess-1", time.Minute, 0, "payload")

	session, state := Poll("sess-1")
	if state != PollReady {
		t.Fatalf("poll state = %v, want ready", state)
	}
	if session == nil || session.VendorID != "vendor-a" || session.Data != "payload" {
		t.Fatalf("session = %+v", session)
	}

	vendorID, okOwns := Owns("sess-1")
	if !okOwns || vendorID != "vendor-a" {
		t.Fatalf("owns = %q %v", vendorID, okOwns)
	}

	Delete("sess-1")
	if _, stateMissing := Poll("sess-1"); stateMissing != PollMissing {
		t.Fatalf("after delete poll state = %v", stateMissing)
	}
	if _, okAfter := Owns("sess-1"); okAfter {
		t.Fatal("deleted session must not be owned")
	}
}

func TestPollThrottling(t *testing.T) {
	Register("vendor-b", "sess-2", time.Minute, time.Hour, nil)

	if _, state := Poll("sess-2"); state != PollReady {
		t.Fatalf("first poll = %v, want ready", state)
	}
	if _, state := Poll("sess-2"); state != PollThrottled {
		t.Fatalf("second poll = %v, want throttled", state)
	}
}

func TestPollExpiry(t *testing.T) {
	Register("vendor-c", "sess-3", time.Millisecond, 0, nil)
	time.Sleep(5 * time.Millisecond)

	if _, state := Poll("sess-3"); state != PollMissing {
		t.Fatalf("expired poll = %v, want missing", state)
	}
	if _, okOwns := Owns("sess-3"); okOwns {
		t.Fatal("expired session must not be owned")
	}
}

func TestPollMissingAndEmptyID(t *testing.T) {
	if _, state := Poll(""); state != PollMissing {
		t.Fatalf("empty id state = %v", state)
	}
	if _, state := Poll("nope"); state != PollMissing {
		t.Fatalf("unknown id state = %v", state)
	}
	// 空 id 不得登记，避免所有空串会话互相覆盖。
	Register("vendor-d", "  ", time.Minute, 0, nil)
	if _, okOwns := Owns(""); okOwns {
		t.Fatal("empty id must not be registered")
	}
}

func TestNewID(t *testing.T) {
	first := NewID(8)
	second := NewID(8)
	if len(first) != 16 {
		t.Fatalf("NewID length = %d, want 16", len(first))
	}
	if first == second {
		t.Fatal("NewID must produce distinct ids")
	}
	if got := NewID(0); len(got) != 32 {
		t.Fatalf("default NewID length = %d, want 32", len(got))
	}
}

func TestConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				id := NewID(4)
				Register("vendor-e", id, time.Minute, 0, nil)
				_, _ = Poll(id)
				_, _ = Owns(id)
				Delete(id)
			}
		}()
	}
	wg.Wait()
}
