package quant

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"live-optimus/backend/internal/risk"
)

// RiseWatch monetizes the bounces the dip pipeline passes on. Agent 2 rejects most dips
// at detection time — and the 3-day replay (2026-07-06..08) showed those rejections are
// individually right (buying at detection: 52% hit the stop first). But 65% of the same
// rejected dips still rose ≥0.5% within 30 minutes; waiting for a CONFIRMED rise before
// entering flipped the edge positive (+0.37R mean across 44 triggers in a hostile tape).
//
// So: every in-universe dip Agent 2 declines is ARMED here for a short window. The rise
// confirms when a completed GREEN 1-min bar closes riseConfirmPct above the dip price
// WITHOUT the dip low ever being undercut and WITHOUT the confirming bar's volume fading
// below the post-dip average. Confirmed rises enter through the SHARED gauntlet tail
// (posture, loss cap, allocator slot/budget) with deterministic, time-boxed exits
// (1.5% trail, +2R target, 40-min max hold).
//
// The rule was validated on 391 dips RECONSTRUCTED by replaying the exact dipwatch recipe
// over a month of 1-min SIP bars (2026-06-08..07-08, 21 sessions): this family made
// +27R/228 entries across BOTH the losing June regime and the winning July one, while the
// earlier 2-green-bar rule (tuned on 5 journal days) lost 11R on the month. The dip-low
// invalidation and the volume check carried the improvement; a VWAP filter and stricter
// multi-bar confirmations tested worse. Walk-forward ML gating (LightGBM + logistic) was
// also tested and HURT at this data size — deterministic stays until the journal is 1k+
// rows. The shadow journal remains the ongoing out-of-sample check.
//
// Deterministic — no LLM on this path; every arm/trigger/expiry is journaled so a future
// classifier can be trained on it. Paper-only by construction (shares the dip pipeline's
// broker/manager); gated by QUANT_RISE_LIVE — default false = shadow (journals would-be
// entries, places nothing).
const (
	riseWatchWindowMin = 10   // dip stays armed this many minutes (bounces either go fast or fail)
	riseConfirmPct     = 0.10 // confirming green close must be this % above the dip price
	riseTrailPct       = 1.5  // trailing stop % (tighter shook out winners)
	riseMaxHoldMin     = 40   // deterministic time exit (shorter cut winners short)
	riseConf           = 0.6  // fixed conviction → half slice (conservative first deployment)
	riseMinRiskPct     = 0.2  // stop-distance floor (% of price) when the dip low is at the price
	riseScanEvery      = 15 * time.Second
	riseEntryCutoffMin = 15*60 + 30 // no fresh entries at/after 15:30 ET (manager flattens 15:55)
)

// risePending is one armed dip waiting for its rise confirmation.
type risePending struct {
	dip     DipEvent
	armedAt time.Time
	conf    float64 // Agent 2's no-buy confidence (journaled for the future classifier)
}

// RiseWatch scans armed dips for a confirmed rise and enters (or shadow-journals) them.
type RiseWatch struct {
	eng    *Engine
	mgr    *Manager
	day    *risk.Day    // shared daily loss-cap tracker (nil-safe: no cap check)
	notify func(string) // Telegram (nil-safe)
	loc    *time.Location
	live   bool

	mu      sync.Mutex
	pending map[string]*risePending
	traded  map[string]bool // symbols already rise-entered today (one shot per name per day)
	day0    string          // ET day the traded set belongs to
}

// NewRiseWatch builds the watcher. live=false keeps it in shadow mode: every trigger is
// journaled and alerted, no orders are placed.
func NewRiseWatch(eng *Engine, mgr *Manager, day *risk.Day, loc *time.Location, live bool, notify func(string)) *RiseWatch {
	if loc == nil {
		loc = time.UTC
	}
	return &RiseWatch{eng: eng, mgr: mgr, day: day, notify: notify, loc: loc, live: live,
		pending: map[string]*risePending{}, traded: map[string]bool{}}
}

// Live reports whether triggers place paper orders (false = shadow).
func (r *RiseWatch) Live() bool { return r != nil && r.live }

// RiseArmView is one armed dip awaiting its rise confirmation (for the Dip+Rise page).
type RiseArmView struct {
	Symbol       string    `json:"symbol"`
	ArmedAt      time.Time `json:"armed_at"`
	DipPrice     float64   `json:"dip_price"`
	DipLow       float64   `json:"dip_low"`
	ConfirmLevel float64   `json:"confirm_level"` // green 1-min close at/above this triggers
	ExpiresInSec int       `json:"expires_in_sec"`
	Agent2Conf   float64   `json:"agent2_conf"` // Agent 2's no-buy confidence when it declined
}

// Armed snapshots the currently armed dips (read-only, for the report/page).
func (r *RiseWatch) Armed() []RiseArmView {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RiseArmView, 0, len(r.pending))
	for _, p := range r.pending {
		exp := int((riseWatchWindowMin*time.Minute - now.Sub(p.armedAt)).Seconds())
		if exp < 0 {
			exp = 0
		}
		out = append(out, RiseArmView{
			Symbol: p.dip.Symbol, ArmedAt: p.armedAt,
			DipPrice: p.dip.Price, DipLow: p.dip.DipLow,
			ConfirmLevel: round2(p.dip.Price * (1 + riseConfirmPct/100)),
			ExpiresInSec: exp, Agent2Conf: p.conf,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArmedAt.Before(out[j].ArmedAt) })
	return out
}

// Arm registers a declined dip for rise-watching. Returns true when armed (so the caller
// can say so in the Telegram label). Re-arming a symbol replaces the older dip — the
// fresher anatomy is the one worth confirming against.
func (r *RiseWatch) Arm(de DipEvent, agent2Conf float64) bool {
	if r == nil {
		return false
	}
	now := time.Now().In(r.loc)
	// Too late for the bounce to be tradable (trigger cutoff is 15:30, window is 20m).
	if now.Hour()*60+now.Minute() >= riseEntryCutoffMin {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rolloverLocked(now)
	if r.traded[de.Symbol] {
		return false // one rise entry per symbol per day
	}
	r.pending[de.Symbol] = &risePending{dip: de, armedAt: time.Now(), conf: agent2Conf}
	r.eng.logRec(LogRecord{Agent: "rise_watch", Event: "arm", Symbol: de.Symbol,
		Note:   fmt.Sprintf("armed %dm: confirm = green 1-min close ≥ $%.2f (+%.2f%%), dip low $%.2f must hold, no volume fade", riseWatchWindowMin, de.Price*(1+riseConfirmPct/100), riseConfirmPct, de.DipLow),
		Output: de})
	return true
}

// Start launches the scan loop.
func (r *RiseWatch) Start(ctx context.Context) {
	if r == nil {
		return
	}
	go func() {
		t := time.NewTicker(riseScanEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.scan(ctx)
			}
		}
	}()
}

// rolloverLocked resets the per-day traded set on the first touch of a new ET day.
func (r *RiseWatch) rolloverLocked(nowET time.Time) {
	if d := nowET.Format("2006-01-02"); d != r.day0 {
		r.day0 = d
		r.traded = map[string]bool{}
		r.pending = map[string]*risePending{}
	}
}

// scan expires stale arms and fires any confirmed rises.
func (r *RiseWatch) scan(ctx context.Context) {
	now := time.Now()
	r.mu.Lock()
	r.rolloverLocked(now.In(r.loc))
	var fire []*risePending
	for sym, p := range r.pending {
		if now.Sub(p.armedAt) > riseWatchWindowMin*time.Minute {
			delete(r.pending, sym)
			r.eng.logRec(LogRecord{Agent: "rise_watch", Event: "skip", Symbol: sym,
				Note: fmt.Sprintf("expired: no confirmed rise within %dm", riseWatchWindowMin)})
			continue
		}
		triggered, dead := r.confirmed(p, now)
		if dead {
			delete(r.pending, sym)
			r.eng.logRec(LogRecord{Agent: "rise_watch", Event: "skip", Symbol: sym,
				Note: "disarmed: dip low undercut before the rise confirmed (bounce thesis dead)"})
			continue
		}
		if triggered {
			delete(r.pending, sym)
			fire = append(fire, p)
		}
	}
	r.mu.Unlock()
	for _, p := range fire {
		r.trigger(ctx, p, now)
	}
}

// confirmed evaluates the COMPLETED 1-min bars since arming (a forming bar can still
// fade). triggered = a green bar closed at/above the confirmation level with volume at
// least the post-dip average (a fading-volume "confirmation" is a trap — the month replay
// showed volume-fade entries lose). dead = some bar undercut the dip low first: the
// bounce thesis is invalidated and the arm should be dropped, not left waiting.
func (r *RiseWatch) confirmed(p *risePending, now time.Time) (triggered, dead bool) {
	level := p.dip.Price * (1 + riseConfirmPct/100)
	var volSum float64
	var volN int
	for _, b := range r.eng.candles.Snapshot(p.dip.Symbol, 1) {
		if b.Time < p.armedAt.Unix() || b.Time+60 > now.Unix() {
			continue
		}
		if b.Low < p.dip.DipLow {
			return false, true
		}
		if b.Close > b.Open && b.Close >= level {
			if volN == 0 || b.Volume >= volSum/float64(volN) {
				return true, false
			}
		}
		volSum += b.Volume
		volN++
	}
	return false, false
}

// trigger runs the confirmed rise through the shared gauntlet tail and enters (live) or
// journals the would-be entry (shadow).
func (r *RiseWatch) trigger(ctx context.Context, p *risePending, now time.Time) {
	sym := p.dip.Symbol
	delayMin := math.Round(now.Sub(p.armedAt).Minutes())
	skip := func(reason string) {
		r.eng.logRec(LogRecord{Agent: "rise_watch", Event: "skip", Symbol: sym,
			Note: fmt.Sprintf("rise confirmed (+%.0fm) but skipped: %s", delayMin, reason)})
		log.Printf("[rise-watch] skip %s — %s", sym, reason)
	}

	nowET := now.In(r.loc)
	if nowET.Hour()*60+nowET.Minute() >= riseEntryCutoffMin {
		skip("too late in the session")
		return
	}
	if r.eng.universe != nil && r.eng.universe.StandDown() {
		skip("regime stand_down")
		return
	}
	if r.day != nil {
		if err := r.day.CanEnter(r.eng.alloc.OpenCount(), now); err != nil {
			skip(err.Error())
			return
		}
	}
	px := r.eng.LastClose(sym)
	if px <= 0 {
		skip("no live price")
		return
	}
	// Risk anchor: the dip low is the invalidation. Stop there, target +2R, and the
	// 25-min max hold caps the trade to the bounce's natural lifetime.
	riskAbs := math.Max(px-p.dip.DipLow, px*riseMinRiskPct/100)
	target := round2(px + 2*riskAbs)

	r.mu.Lock()
	already := r.traded[sym]
	r.traded[sym] = true
	r.mu.Unlock()
	if already {
		skip("already rise-traded today")
		return
	}

	if !r.live {
		size := r.eng.alloc.Size(riseConf)
		r.eng.logRec(LogRecord{Agent: "rise_watch", Event: "skip", Symbol: sym,
			Note: fmt.Sprintf("SHADOW: rise confirmed +%.0fm after dip — would enter $%.0f @ $%.2f (stop $%.2f, target $%.2f, trail %.1f%%, max %dm)",
				delayMin, size, px, p.dip.DipLow, target, riseTrailPct, riseMaxHoldMin)})
		r.send(fmt.Sprintf("📈 %s rise confirmed %.0fm after the dip — $%.2f (shadow: would buy $%.0f, stop $%.2f, target $%.2f)",
			sym, delayMin, px, size, p.dip.DipLow, target))
		return
	}

	if !r.eng.alloc.CanFund(sym) {
		skip("no slot/capital free (or already held)")
		return
	}
	size := r.eng.alloc.Size(riseConf)
	if size <= 0 || math.Floor(size/px) < 1 {
		skip(fmt.Sprintf("$%.0f can't buy 1 share at $%.2f", size, px))
		return
	}
	if !r.eng.alloc.Fund(sym, size) {
		skip("slot taken while deciding")
		return
	}
	r.eng.logRec(LogRecord{Agent: "rise_watch", Event: "order", Symbol: sym,
		Note: fmt.Sprintf("rise confirmed +%.0fm: funded $%.0f @ $%.2f (stop $%.2f, target $%.2f, trail %.1f%%, max %dm)",
			delayMin, size, px, p.dip.DipLow, target, riseTrailPct, riseMaxHoldMin)})
	log.Printf("[rise-watch] ENTER %s $%.0f @ $%.2f (rise confirmed %.0fm after dip)", sym, size, px, delayMin)
	r.send(fmt.Sprintf("📈 %s rise confirmed %.0fm after the dip — buying $%.0f @ $%.2f (stop $%.2f, target $%.2f, max %dm)",
		sym, delayMin, size, px, p.dip.DipLow, target, riseMaxHoldMin))
	go r.mgr.OpenPosition(ctx, sym, riseConf, size, EntryContext{
		Source: "rise", Strategy: "dip_rise",
		Target: target, Stop: p.dip.DipLow,
		TrailPct: riseTrailPct, MaxHoldMin: riseMaxHoldMin,
	})
}

func (r *RiseWatch) send(text string) {
	if r.notify != nil {
		r.notify(text)
	}
}
