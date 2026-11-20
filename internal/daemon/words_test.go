package daemon

import "testing"

func TestCount(t *testing.T) {
	for n, want := range map[int]string{0: "0 secrets", 1: "1 secret", 2: "2 secrets", 14: "14 secrets"} {
		if got := count(n, "secret"); got != want {
			t.Errorf("count(%d) = %q, want %q", n, got, want)
		}
	}
}
