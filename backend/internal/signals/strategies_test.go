package signals

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkSession builds a synthetic regular-session bar series starting at 09:30 ET.
func sessionOpenUnix(t *testing.T) int64 {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// A fixed weekday (Mon 2026-06-15) so the session math is deterministic.
	return time.Date(2026, 6, 15, 9, 30, 0, 0, loc).Unix()
}

func bar(open int64, minute int, o, h, l, c, v float64) Bar {
	return Bar{Time: open + int64(minute)*60, Open: o, High: h, Low: l, Close: c, Volume: v}
}

// flatBars returns n quiet bars oscillating around px.
func flatBars(open int64, n int, px, vol float64) []Bar {
	out := make([]Bar, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, bar(open, i, px, px+0.05, px-0.05, px, vol))
	}
	return out
}

func baseCtx(open int64) Context {
	return Context{SessionOpen: open, ATR: 2.0, AvgVolume: 5_000_000, RVOL: 2.0, MarketOK: true, MarketPct: 0.4}
}

func TestORBBreakoutFires(t *testing.T) {
	open := sessionOpenUnix(t)
	// First 15 minutes range 100–101, then drift below the OR high, then break out.
	bars := []Bar{}
	for i := 0; i < 15; i++ {
		bars = append(bars, bar(open, i, 100.2, 101.0, 100.0, 100.5, 10_000))
	}
	for i := 15; i < 25; i++ {
		bars = append(bars, bar(open, i, 100.5, 100.9, 100.3, 100.6, 10_000))
	}
	// The breakout bar: closes above 101 on 3× volume.
	bars = append(bars, bar(open, 25, 100.9, 101.5, 100.8, 101.3, 30_000))

	sig := (ORBBreakout{}).Detect("AAA", bars, baseCtx(open))
	if sig == nil {
		t.Fatal("expected an ORB breakout signal")
	}
	if sig.Suggested.Stop >= sig.Suggested.Entry || sig.Suggested.Target <= sig.Suggested.Entry {
		t.Fatalf("bad bracket: %+v", sig.Suggested)
	}

	// Same setup with a risk-off market must NOT fire.
	ctx := baseCtx(open)
	ctx.MarketOK = false
	if (ORBBreakout{}).Detect("AAA", bars, ctx) != nil {
		t.Fatal("ORB must not fire against a risk-off market")
	}
	// Nor on quiet volume.
	ctx = baseCtx(open)
	ctx.RVOL = 0.8
	if (ORBBreakout{}).Detect("AAA", bars, ctx) != nil {
		t.Fatal("ORB must not fire on low RVOL")
	}
}

func TestQuietTapeProducesNoSignals(t *testing.T) {
	open := sessionOpenUnix(t)
	bars := flatBars(open, 120, 100, 10_000)
	ctx := baseCtx(open)
	ctx.RVOL = 0.9
	for _, st := range DefaultStrategies() {
		if sig := st.Detect("AAA", bars, ctx); sig != nil {
			t.Fatalf("%s fired on a flat, quiet tape: %+v", st.Name(), sig)
		}
	}
}

func TestVWAPReclaimFires(t *testing.T) {
	open := sessionOpenUnix(t)
	bars := []Bar{}
	// 30 minutes of gentle two-way tape around $100 (realistic RSI baseline ≈ 50).
	for i := 0; i < 30; i++ {
		c := 100.0
		if i%2 == 1 {
			c = 100.1
		}
		bars = append(bars, bar(open, i, 100.0, 100.15, 99.95, c, 20_000))
	}
	// The flush: a sharp drop then a grind lower, well below VWAP (≈1×ATR under).
	for i := 30; i < 42; i++ {
		c := 99.30 - 0.04*float64(i-30)
		bars = append(bars, bar(open, i, c+0.05, c+0.07, c-0.03, c, 25_000))
	}
	// A tentative recovery off the low, still below VWAP.
	for i := 42; i < 48; i++ {
		c := 98.91 + 0.05*float64(i-42)
		bars = append(bars, bar(open, i, c-0.03, c+0.05, c-0.02, c, 22_000))
	}
	// The reclaim bar: strong green close back above the session VWAP (~99.63).
	bars = append(bars, bar(open, 48, 99.30, 99.80, 99.25, 99.75, 30_000))

	sig := (VWAPReclaim{}).Detect("AAA", bars, baseCtx(open))
	if sig == nil {
		t.Fatal("expected a VWAP reclaim signal")
	}
	if sig.Suggested.Stop >= sig.Suggested.Entry {
		t.Fatalf("stop must sit below entry: %+v", sig.Suggested)
	}
}

func TestFirstHourReversalFires(t *testing.T) {
	open := sessionOpenUnix(t)
	bars := []Bar{}
	// Morning dump: $100 open bleeding to $97.5 by minute 29 (1.25 × ATR(2)).
	for i := 0; i < 30; i++ {
		c := 100.0 - 2.5*float64(i)/29.0
		bars = append(bars, bar(open, i, c+0.05, c+0.1, c-0.05, c, 30_000))
	}
	// The low stops going down: half an hour of basing above the low.
	for i := 30; i < 65; i++ {
		c := 97.6 + 0.2*float64(i%3)/2
		bars = append(bars, bar(open, i, c-0.02, c+0.06, c-0.04, c, 20_000))
	}
	// 10:35 ET: a green bar lifting clearly off the low → reversal entry.
	bars = append(bars, bar(open, 65, 97.95, 98.30, 97.90, 98.25, 28_000))

	sig := (FirstHourReversal{}).Detect("AAA", bars, baseCtx(open))
	if sig == nil {
		t.Fatal("expected a first-hour reversal signal")
	}
	if sig.Suggested.Stop >= sig.Suggested.Entry || sig.Suggested.Target <= sig.Suggested.Entry {
		t.Fatalf("bad bracket: %+v", sig.Suggested)
	}
	// Same tape 30 minutes earlier (minute 35) must NOT fire — outside the window.
	if (FirstHourReversal{}).Detect("AAA", bars[:36], baseCtx(open)) != nil {
		t.Fatal("reversal must not fire before 10:30 ET")
	}
}

func TestEngineTODSeedGatesEntries(t *testing.T) {
	dir := t.TempDir()
	// Seed: dip_bounce's 11:00 bucket (index 3) proven negative; bucket 0 healthy.
	if err := os.MkdirAll(filepath.Join(dir, "signals"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"dip_bounce|3":{"n":100,"sum":-8.5},"dip_bounce|0":{"n":100,"sum":12.0}}`
	if err := os.WriteFile(filepath.Join(dir, "signals", "tod_stats.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	uni := &Universe{ContextSymbols: []string{"QQQ"}, sectorOf: map[string]string{"AAA": "test"}}
	e := NewEngine(uni, dir)

	open := sessionOpenUnix(t)
	// Prime the store's session clock with one bar.
	e.OnBar("AAA", time.Unix(open, 0), 100, 100.1, 99.9, 100, 1000)

	blocked := Signal{Strategy: "dip_bounce", Time: open + 3*30*60 + 60} // 11:01 ET → bucket 3
	healthy := Signal{Strategy: "dip_bounce", Time: open + 60}           // 09:31 ET → bucket 0
	if e.EntryAllowed(blocked) {
		t.Fatal("seeded-negative bucket must block entries")
	}
	if !e.EntryAllowed(healthy) {
		t.Fatal("healthy bucket must allow entries")
	}
	if e.EntryAllowed(Signal{Strategy: "vwap_reclaim", Time: blocked.Time}) != true {
		t.Fatal("unseeded strategy/bucket must pass through")
	}
}

func TestEngineCooldownAndOutcome(t *testing.T) {
	uni := &Universe{ContextSymbols: []string{"QQQ"}, Sectors: map[string][]string{"test": {"AAA"}},
		sectorOf: map[string]string{"AAA": "test"}}
	e := NewEngine(uni, t.TempDir())
	e.SeedDaily("AAA", 2.0, 5_000_000)

	open := sessionOpenUnix(t)
	var published []Signal
	e.OnSignal = func(s Signal) { published = append(published, s) }

	// Drive the ORB setup through the live path twice; the cooldown must swallow the second.
	feed := func(minShift int) {
		for i := 0; i < 15; i++ {
			e.OnBar("AAA", time.Unix(open+int64(i)*60, 0), 100.2, 101.0, 100.0, 100.5, 900_000)
		}
		for i := 15; i < 25+minShift; i++ {
			e.OnBar("AAA", time.Unix(open+int64(i)*60, 0), 100.5, 100.9, 100.3, 100.6, 900_000)
		}
		e.OnBar("AAA", time.Unix(open+int64(25+minShift)*60, 0), 100.9, 101.5, 100.8, 101.3, 3_000_000)
	}
	feed(0)
	if len(published) != 1 {
		t.Fatalf("expected 1 published signal, got %d", len(published))
	}
	// Two minutes later the same cross would fire again — cooldown must block it.
	e.OnBar("AAA", time.Unix(open+26*60, 0), 101.0, 101.0, 100.9, 100.95, 900_000)
	e.OnBar("AAA", time.Unix(open+27*60, 0), 100.9, 101.6, 100.9, 101.4, 3_000_000)
	if len(published) != 1 {
		t.Fatalf("cooldown failed: %d signals", len(published))
	}

	// A bar through the target must resolve the pending counterfactual outcome.
	target := published[0].Suggested.Target
	e.OnBar("AAA", time.Unix(open+30*60, 0), target, target+0.5, target-0.1, target+0.3, 900_000)
	e.mu.Lock()
	pendingLeft := 0
	for _, p := range e.pending {
		if !p.done {
			pendingLeft++
		}
	}
	e.mu.Unlock()
	if pendingLeft != 0 {
		t.Fatalf("outcome not resolved; %d pending", pendingLeft)
	}
}

func TestEngineExtraFeaturesMergedIntoPublishedSignal(t *testing.T) {
	uni := &Universe{ContextSymbols: []string{"QQQ"}, Sectors: map[string][]string{"test": {"AAA"}},
		sectorOf: map[string]string{"AAA": "test"}}
	e := NewEngine(uni, t.TempDir())
	e.SeedDaily("AAA", 2.0, 5_000_000)

	open := sessionOpenUnix(t)
	var published []Signal
	e.OnSignal = func(s Signal) { published = append(published, s) }
	e.SetExtraFeatures(func(sym string) map[string]float64 {
		return map[string]float64{"spread_bps": 4.2, "flow_delta_5m": 1234.0, "flow_buy_frac": 0.6}
	})

	for i := 0; i < 15; i++ {
		e.OnBar("AAA", time.Unix(open+int64(i)*60, 0), 100.2, 101.0, 100.0, 100.5, 900_000)
	}
	for i := 15; i < 25; i++ {
		e.OnBar("AAA", time.Unix(open+int64(i)*60, 0), 100.5, 100.9, 100.3, 100.6, 900_000)
	}
	e.OnBar("AAA", time.Unix(open+25*60, 0), 100.9, 101.5, 100.8, 101.3, 3_000_000)

	if len(published) != 1 {
		t.Fatalf("expected 1 published signal, got %d", len(published))
	}
	f := published[0].Features
	if f["spread_bps"] != 4.2 || f["flow_delta_5m"] != 1234.0 || f["flow_buy_frac"] != 0.6 {
		t.Fatalf("extra features not merged into published signal: %+v", f)
	}
}
