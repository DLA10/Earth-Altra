package signals

import "testing"

// TestLoadRepoUniverse parses the REAL QUANT_UNIVERSE.json with the production loader.
// This is the guard against the 2026-07-17 near-miss: the file was regenerated with a
// UTF-8 BOM that Go's json.Unmarshal rejects — every `go test` was green while the
// backend would have booted with NO universe (signal engine + RIDP dead). Any encoding,
// shape, or shrink regression in the file must fail here, not at market open.
func TestLoadRepoUniverse(t *testing.T) {
	u, err := LoadUniverse("../../../QUANT_UNIVERSE.json")
	if err != nil {
		t.Fatalf("QUANT_UNIVERSE.json failed to parse with the production loader: %v", err)
	}
	if n := len(u.Symbols()); n < 500 {
		t.Fatalf("universe has %d symbols — expected 500+ after the 2026-07-16 S&P expansion", n)
	}
	for _, want := range []string{"SKHY", "SPCX", "SNDK", "NVDA", "MMM"} {
		if !u.Has(want) {
			t.Errorf("expected %s in the universe, not found", want)
		}
	}
	for _, gone := range []string{"PSTG", "CFLT"} {
		if u.Has(gone) {
			t.Errorf("%s is delisted/untradable and should have been removed", gone)
		}
	}
	if len(u.Context()) == 0 {
		t.Error("context symbols (SPY/QQQ/SMH) missing")
	}
}
