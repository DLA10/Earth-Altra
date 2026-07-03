package signals

import "sort"

// return15m computes a symbol's percentage return over the trailing 15 minutes of a
// chronological bar slice: (latest close − close of the last bar at/before now−15m) /
// that close. ok=false if fewer than 15 minutes of history exist yet.
func return15m(bars []Bar) (float64, bool) {
	n := len(bars)
	if n == 0 {
		return 0, false
	}
	now := bars[n-1]
	cutoff := now.Time - 15*60
	// bars is chronological; find the last index with Time <= cutoff via binary search.
	i := sort.Search(n, func(i int) bool { return bars[i].Time > cutoff }) - 1
	if i < 0 || bars[i].Close <= 0 {
		return 0, false
	}
	return (now.Close - bars[i].Close) / bars[i].Close, true
}

// sectorLeadLagFeatures computes sector_ret_15m (mean 15-min return of sym's OTHER
// same-sector universe peers) and peer_gap_15m (that mean minus sym's own 15-min return)
// — P2.1 (RESEARCH_BACKLOG #9). getBars returns a symbol's session bars so far today (the
// live Store or the backtester's per-day session map — same shape, different source).
// A peer with under 15 minutes of history is skipped; returns nil if sym has no sector,
// its own return isn't computable yet, or no peer has enough history.
func sectorLeadLagFeatures(uni *Universe, sym string, getBars func(string) []Bar) map[string]float64 {
	ownRet, ok := return15m(getBars(sym))
	if !ok {
		return nil
	}
	sector := uni.Sector(sym)
	if sector == "" {
		return nil
	}
	var sum float64
	var n int
	for _, peer := range uni.Sectors[sector] {
		if peer == sym {
			continue
		}
		if r, ok := return15m(getBars(peer)); ok {
			sum += r
			n++
		}
	}
	if n == 0 {
		return nil
	}
	sectorRet := sum / float64(n)
	return map[string]float64{
		"sector_ret_15m": sectorRet,
		"peer_gap_15m":   sectorRet - ownRet,
	}
}
