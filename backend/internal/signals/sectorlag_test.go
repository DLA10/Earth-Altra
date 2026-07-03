package signals

import "testing"

func synthBars(open int64, n int, startPx, drift float64) []Bar {
	out := make([]Bar, 0, n)
	px := startPx
	for i := 0; i < n; i++ {
		px += drift
		out = append(out, Bar{Time: open + int64(i)*60, Open: px, High: px + 0.05, Low: px - 0.05, Close: px, Volume: 1000})
	}
	return out
}

func TestReturn15m(t *testing.T) {
	open := sessionOpenUnix(t)
	// 20 bars, price rises steadily from 100 to 100+20*0.1=102.0; the 15-min-ago bar is
	// bars[4] (t=open+4*60) since "now" is bars[19] (t=open+19*60), cutoff=open+19*60-900=open+4*60.
	bars := synthBars(open, 20, 100, 0.1)
	r, ok := return15m(bars)
	if !ok {
		t.Fatal("expected ok=true with 20 bars of history")
	}
	base := bars[4].Close
	now := bars[19].Close
	want := (now - base) / base
	if diff := r - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("return15m = %v, want %v", r, want)
	}

	if _, ok := return15m(bars[:5]); ok {
		t.Error("expected ok=false with under 15 minutes of history")
	}
	if _, ok := return15m(nil); ok {
		t.Error("expected ok=false on empty bars")
	}
}

func TestSectorLeadLagFeatures(t *testing.T) {
	open := sessionOpenUnix(t)
	uni := &Universe{Sectors: map[string][]string{"semis": {"AAA", "BBB", "CCC"}}}
	uni.sectorOf = map[string]string{"AAA": "semis", "BBB": "semis", "CCC": "semis"}

	sessions := map[string][]Bar{
		"AAA": synthBars(open, 20, 100, 0.0),  // flat: 0% return
		"BBB": synthBars(open, 20, 100, 0.2),  // rising peer
		"CCC": synthBars(open, 20, 100, -0.2), // falling peer
	}
	get := func(s string) []Bar { return sessions[s] }

	ownRet, _ := return15m(sessions["AAA"])
	bbbRet, _ := return15m(sessions["BBB"])
	cccRet, _ := return15m(sessions["CCC"])
	wantSector := (bbbRet + cccRet) / 2
	wantGap := wantSector - ownRet

	feats := sectorLeadLagFeatures(uni, "AAA", get)
	if feats == nil {
		t.Fatal("expected non-nil features")
	}
	const eps = 1e-9
	if diff := feats["sector_ret_15m"] - wantSector; diff > eps || diff < -eps {
		t.Errorf("sector_ret_15m = %v, want %v", feats["sector_ret_15m"], wantSector)
	}
	if diff := feats["peer_gap_15m"] - wantGap; diff > eps || diff < -eps {
		t.Errorf("peer_gap_15m = %v, want %v", feats["peer_gap_15m"], wantGap)
	}

	// No sector → nil.
	if f := sectorLeadLagFeatures(uni, "ZZZ", get); f != nil {
		t.Errorf("expected nil for symbol with no sector, got %+v", f)
	}
	// Insufficient own history → nil.
	sessions["AAA"] = sessions["AAA"][:5]
	if f := sectorLeadLagFeatures(uni, "AAA", get); f != nil {
		t.Errorf("expected nil when own return isn't computable yet, got %+v", f)
	}
}
