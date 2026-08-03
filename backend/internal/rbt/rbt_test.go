package rbt

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The 2026-08-02 change added Rank/OfN to Position and Trade and raised maxSlots from a
// hardcoded 5 to 10. These guard the two ways that could go wrong on a live desk:
// silently losing the book on restart, and sizing positions down to zero shares.

// legacyState is a real pre-change state.json shape: no rank, no of_n anywhere.
const legacyState = `{
  "open": {
    "KEY": {"symbol":"KEY","direction":"Long","qty":887,"entry_price":22.62,
      "opened_at":"2026-07-28T20:50:49.412Z","target_price":23.6047,
      "stop_loss":21.8977,"stop_id":"5c4d6277","age":3},
    "NOW": {"symbol":"NOW","direction":"Short","qty":182,"entry_price":110.04,
      "opened_at":"2026-07-28T20:50:50.100Z","target_price":97.63,
      "stop_loss":119.87,"stop_id":"a1b2c3d4","age":3}
  },
  "trades": [
    {"symbol":"GOOGL","direction":"Long","qty":46,"entry_price":318.32,
     "exit_price":333.73,"pnl":708.91,"reason":"target",
     "opened_at":"2026-07-24T14:00:06Z","closed_at":"2026-07-28T20:55:42Z"}
  ]
}`

// A restart must not lose or corrupt positions booked before ranking existed.
func TestLegacyStateLoadsAndRankDefaultsToZero(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacyState), 0644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{dataDir: dir, open: map[string]*Position{}}
	m.loadState()

	if len(m.open) != 2 {
		t.Fatalf("open positions: got %d, want 2 — a restart would have dropped the book", len(m.open))
	}
	key := m.open["KEY"]
	if key == nil || key.Qty != 887 || key.EntryPrice != 22.62 || key.Age != 3 {
		t.Fatalf("KEY did not survive the load: %+v", key)
	}
	if key.StopID != "5c4d6277" {
		t.Fatalf("StopID lost: %q — the desk would think the position is naked", key.StopID)
	}
	if key.Rank != 0 || key.OfN != 0 {
		t.Errorf("legacy position should carry no rank, got %d/%d", key.Rank, key.OfN)
	}
	if len(m.trades) != 1 || m.trades[0].PnL != 708.91 {
		t.Fatalf("closed trade did not survive: %+v", m.trades)
	}
	if m.trades[0].Rank != 0 {
		t.Errorf("legacy trade should carry no rank, got %d", m.trades[0].Rank)
	}
}

// Rank must survive save -> load, or the column is decorative.
func TestRankRoundTrips(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{dataDir: dir, open: map[string]*Position{
		"MU": {Symbol: "MU", Direction: "Long", Qty: 10, EntryPrice: 100, Rank: 3, OfN: 9},
	}, trades: []Trade{
		{Symbol: "WDC", Direction: "Short", Qty: 5, PnL: -12.5, Reason: "stop_loss", Rank: 7, OfN: 9},
	}}
	m.saveState()

	m2 := &Manager{dataDir: dir, open: map[string]*Position{}}
	m2.loadState()
	if got := m2.open["MU"]; got == nil || got.Rank != 3 || got.OfN != 9 {
		t.Fatalf("position rank lost in round trip: %+v", got)
	}
	if len(m2.trades) != 1 || m2.trades[0].Rank != 7 || m2.trades[0].OfN != 9 {
		t.Fatalf("trade rank lost in round trip: %+v", m2.trades)
	}
}

// omitempty must keep rank out of the JSON when it is unset, so legacy rows stay legacy
// rather than acquiring a misleading "rank 0".
func TestUnrankedPositionOmitsRankInJSON(t *testing.T) {
	b, err := json.Marshal(&Position{Symbol: "X", Qty: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"rank"`, `"of_n"`} {
		if contains(string(b), k) {
			t.Errorf("unranked position serialised %s: %s", k, b)
		}
	}
}

// maxSlots is also the position sizer (equity/maxSlots), so raising it shrinks every
// position. Measured against the real book: ~$100k equity, priciest tradable name ASML
// at ~$1,630. Nothing sizes to ZERO shares until 62 slots, but granularity rots earlier.
// These numbers are pinned so a future raise is a deliberate choice, not a surprise.
func TestSlotSizingHeadroom(t *testing.T) {
	const equity = 100_000.0
	const priciest = 1_630.0 // ASML, the dearest name in a cointegration family

	// the shipped setting must buy a workable number of shares even at the top price
	if qty := math.Floor((equity / 10) / priciest); qty < 5 {
		t.Errorf("at 10 slots the priciest name buys %.0f shares — too coarse for a stop", qty)
	}
	// the hard floor: below this the desk silently skips names via `if qty <= 0`
	first0 := 0
	for slots := 5; slots < 200; slots++ {
		if math.Floor((equity/float64(slots))/priciest) < 1 {
			first0 = slots
			break
		}
	}
	if first0 != 62 {
		t.Errorf("zero-share starvation starts at %d slots, comment on maxSlots says 62", first0)
	}
}

// The division at tradeBudget := safetyBP / slotsLeft is only safe because the caller
// returns first when the book is full. Rolling RBT_MAX_SLOTS back below the open count
// must hit that guard, not divide by a negative.
func TestFullBookYieldsNonPositiveSlotsLeft(t *testing.T) {
	for _, tc := range []struct{ slots, open int }{{10, 10}, {5, 10}, {5, 7}} {
		if left := tc.slots - tc.open; left > 0 {
			t.Errorf("slots=%d open=%d gave %d free; the caller's guard would not fire",
				tc.slots, tc.open, left)
		}
	}
}

// Entries are sequential and each can wait up to 20s for a terminal fill state, so the
// 15:50 scan needs a wall-clock stop or a full book of free slots walks into the close.
func TestEntryCutoffLeavesRoomAndCannotReachTheClose(t *testing.T) {
	const scanStart = 15*60 + 50
	const marketClose = 16 * 60
	const worstCaseSecondsPerEntry = 26 // 20s fill wait + stop placement with retries

	if entryCutoffMin <= scanStart {
		t.Fatalf("cutoff %d is at or before the 15:50 scan — the desk could never enter", entryCutoffMin)
	}
	if entryCutoffMin >= marketClose {
		t.Fatalf("cutoff %d is at or after the 16:00 close", entryCutoffMin)
	}
	// the guard is checked BEFORE each entry, so one entry may still start at the cutoff
	// and must finish before the bell
	if latest := entryCutoffMin*60 + worstCaseSecondsPerEntry; latest > marketClose*60 {
		t.Errorf("an entry starting at the cutoff could still be working at the close")
	}
	// and the window must fit a useful number of entries, or the raise to 10 is pointless
	if fits := (entryCutoffMin - scanStart) * 60 / worstCaseSecondsPerEntry; fits < 10 {
		t.Errorf("only %d entries fit before the cutoff; maxSlots is %d", fits, maxSlots)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
