package tasks

import "testing"

func TestNormalizeHoursAndNextWake(t *testing.T) {
	if got := NormalizeHours([]int{9, 9, 25, -1, 21}); len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Fatalf("NormalizeHours = %#v", got)
	}
}

func TestNormalizeHoursKeepsValidValues(t *testing.T) {
	if got := NormalizeHours([]int{0, 23}); len(got) != 2 || got[0] != 0 || got[1] != 23 {
		t.Fatalf("NormalizeHours = %#v", got)
	}
}
