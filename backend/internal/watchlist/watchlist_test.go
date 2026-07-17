package watchlist

import "testing"

// TestLoadRepoWatchlist parses the real EVENT_DRIVEN_WATCHLIST.md and sanity-checks the
// 2026-07-16 expansion (full S&P 500 core coverage + new listings). Guards against a
// formatting slip silently shrinking the scanner/dipwatch universe.
func TestLoadRepoWatchlist(t *testing.T) {
	wl, err := Load("../../../EVENT_DRIVEN_WATCHLIST.md")
	if err != nil {
		t.Skipf("watchlist file not found: %v", err)
	}
	if n := len(wl.Symbols); n < 650 {
		t.Fatalf("parsed only %d symbols — expected 650+ after the S&P 500 expansion", n)
	}
	if n := len(wl.Departments); n < 30 {
		t.Fatalf("parsed only %d departments — expected 30+ (S&P 500 core sectors added)", n)
	}
	t.Logf("watchlist: %d departments, %d unique symbols", len(wl.Departments), len(wl.Symbols))
	want := []string{"SKHY", "SPCX", "SNDK", "MMM", "XOM"}
	have := map[string]bool{}
	for _, s := range wl.Symbols {
		have[s] = true
	}
	for _, s := range want {
		if !have[s] {
			t.Errorf("expected %s in the parsed universe, not found", s)
		}
	}
}
