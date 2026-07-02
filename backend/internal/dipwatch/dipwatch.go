// Package dipwatch sends Telegram alerts when a watched stock dips into an oversold,
// below-VWAP pullback and then prints its first green bounce candle. It is a read-only
// observer: it reads the in-memory candle engine + Alpaca daily bars/news and POSTs to
// Telegram. It NEVER places orders or touches the execution/stream path.
//
// Recipe (per operator): a meaningful pullback (>= ~0.5x daily ATR off the high) + oversold
// (RSI <= threshold or under the lower Bollinger band) + below VWAP/open + busy (RVOL) +
// no fresh negative news, confirmed by ONE completed green 5-minute candle. 15-min cooldown.
package dipwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"live-optimus/backend/internal/alpaca"
	"live-optimus/backend/internal/candles"
)

// Tunables (kept as constants for the test; easy to lift into config later).
const (
	bounceTF        = 5    // minutes — wait for one completed 5-min candle (operator's choice)
	rsiOversold     = 35.0 // 1-min RSI at/below this counts as oversold (slightly relaxed to catch a bouncing dip)
	pullbackATRfrac = 0.5  // dip must be >= this fraction of the stock's daily ATR off the high
	minRVOL         = 1.5  // relative volume floor (stock is "in play")
	vwapWarmupMin   = 15   // ignore the first N minutes — VWAP isn't trustworthy yet
	cooldown        = 15 * time.Minute
	scanEvery       = 20 * time.Second
)

// Watcher evaluates the dip recipe for a set of symbols and alerts via Telegram. The symbol
// set is supplied by symbolsFn, evaluated each scan, so it always reflects the current
// watchlist (including symbols the user adds at runtime).
type Watcher struct {
	token     string
	chatID    string
	symbolsFn func() []string
	engine    *candles.Engine
	client    *alpaca.Client
	http      *http.Client
	hook      DipHook // optional: the quant Agent-2 pipeline; returns a label for the alert

	mu       sync.Mutex
	atr      map[string]float64   // daily ATR(14) per symbol
	avgVol   map[string]float64   // avg daily volume per symbol
	lastBar  map[string]int64     // last evaluated 5-min bar time per symbol (dedupe)
	cooldown map[string]time.Time // last alert time per symbol
	statDay  string               // ET date the daily stats were computed for
}

// New builds a Watcher. symbolsFn supplies the current symbol set on each scan (e.g. the live
// watchlist). It is disabled (Enabled() == false) unless a token, chat ID, symbol provider,
// and engine are configured.
func New(token, chatID string, symbolsFn func() []string, engine *candles.Engine, client *alpaca.Client) *Watcher {
	return &Watcher{
		token:     strings.TrimSpace(token),
		chatID:    strings.TrimSpace(chatID),
		symbolsFn: symbolsFn,
		engine:    engine,
		client:    client,
		http:      &http.Client{Timeout: 10 * time.Second},
		atr:       map[string]float64{},
		avgVol:    map[string]float64{},
		lastBar:   map[string]int64{},
		cooldown:  map[string]time.Time{},
	}
}

// DipSignal is the data the watcher hands to an optional hook (the quant pipeline) when a dip is
// confirmed. The hook returns a short label that gets appended to the Telegram alert — so a
// SINGLE detector serves both the broad alert and the agent pipeline (no divergence/confusion).
type DipSignal struct {
	Symbol       string
	Time         time.Time
	Price        float64
	DayHigh      float64
	DipLow       float64
	ATR          float64
	RSI          float64
	VWAP         float64
	DayOpen      float64
	RVOL         float64
	BounceVolume float64
	NegativeNews bool
}

// DipHook is called on every confirmed dip; its returned label is appended to the alert.
type DipHook func(DipSignal) string

// SetHook installs the dip hook (e.g. the quant Agent-2 entry pipeline).
func (w *Watcher) SetHook(h DipHook) { w.hook = h }

// symbols returns the current symbol set from the provider (nil-safe).
func (w *Watcher) symbols() []string {
	if w.symbolsFn == nil {
		return nil
	}
	return w.symbolsFn()
}

// Enabled reports whether the watcher has everything it needs to run.
func (w *Watcher) Enabled() bool {
	return w != nil && w.token != "" && w.chatID != "" && w.symbolsFn != nil && w.engine != nil
}

// Start launches the background scan loop (no-op if disabled).
func (w *Watcher) Start(ctx context.Context) {
	if !w.Enabled() {
		return
	}
	go w.loop(ctx)
}

func (w *Watcher) loop(ctx context.Context) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	t := time.NewTicker(scanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			et := now.In(loc)
			day := et.Format("2006-01-02")
			if day != w.statDay {
				w.computeDailyStats() // ATR + avg volume from history
				w.mu.Lock()
				w.lastBar = map[string]int64{}
				w.cooldown = map[string]time.Time{}
				w.statDay = day
				w.mu.Unlock()
			}
			if !regularHours(et) {
				continue
			}
			syms := w.symbols()
			w.ensureStats(syms) // lazily seed ATR/avg-vol for symbols added since the daily compute
			sessionStart := time.Date(et.Year(), et.Month(), et.Day(), 9, 30, 0, 0, loc).Unix()
			for _, sym := range syms {
				w.checkSymbol(sym, now, sessionStart)
			}
		}
	}
}

// computeDailyStats pulls ~20 daily bars per symbol and computes ATR(14) + avg daily volume
// for the current symbol set.
func (w *Watcher) computeDailyStats() {
	w.seedStats(w.symbols())
}

// ensureStats computes daily stats for any of `syms` that don't yet have them — so a symbol
// added to the watchlist mid-session starts being evaluated without waiting for the next day.
func (w *Watcher) ensureStats(syms []string) {
	var missing []string
	w.mu.Lock()
	for _, sym := range syms {
		if _, ok := w.atr[sym]; !ok {
			missing = append(missing, sym)
		}
	}
	w.mu.Unlock()
	if len(missing) > 0 {
		w.seedStats(missing)
	}
}

// seedStats fetches daily bars and computes ATR(14) + avg daily volume for the given symbols.
func (w *Watcher) seedStats(syms []string) {
	if len(syms) == 0 {
		return
	}
	daily, err := w.client.GetMultiDailyBars(syms, 20)
	if err != nil {
		log.Printf("dipwatch: daily stats error: %v", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for sym, bars := range daily {
		if len(bars) < 2 {
			continue
		}
		var highs, lows, closes, vols []float64
		for _, b := range bars {
			highs = append(highs, b.High)
			lows = append(lows, b.Low)
			closes = append(closes, b.Close)
			vols = append(vols, b.Volume)
		}
		w.atr[sym] = atr14(highs, lows, closes)
		w.avgVol[sym] = avgLast(vols, 14)
	}
}

// checkSymbol evaluates one symbol on the latest COMPLETED 5-minute candle.
func (w *Watcher) checkSymbol(sym string, now time.Time, sessionStart int64) {
	bars5 := w.engine.Snapshot(sym, bounceTF)
	if len(bars5) < 2 {
		return
	}
	// Latest fully-closed 5-min bar (its window has elapsed).
	ci := -1
	for i := len(bars5) - 1; i >= 0; i-- {
		if bars5[i].Time+int64(bounceTF*60) <= now.Unix() {
			ci = i
			break
		}
	}
	if ci < 1 {
		return
	}
	cur, prev := bars5[ci], bars5[ci-1]

	// Dedupe: only evaluate a given 5-min bar once.
	w.mu.Lock()
	if w.lastBar[sym] == cur.Time {
		w.mu.Unlock()
		return
	}
	w.lastBar[sym] = cur.Time
	cdActive := !w.cooldown[sym].IsZero() && time.Since(w.cooldown[sym]) < cooldown
	atr := w.atr[sym]
	avgVol := w.avgVol[sym]
	w.mu.Unlock()

	// 1-minute session series for RSI / Bollinger / VWAP / day-high / day-open / volume.
	closes1, dayHigh, dayOpen, vwap, todayVol := w.sessionStats(sym, sessionStart)
	if len(closes1) < 20 {
		return // not enough data yet (need ~20 one-minute bars)
	}
	elapsedMin := now.Sub(time.Unix(sessionStart, 0)).Minutes()
	if elapsedMin < vwapWarmupMin {
		return
	}

	r := rsi(closes1, 14)
	lower, okBoll := bollingerLower(closes1, 20, 2)
	frac := elapsedMin / 390.0
	if frac < 0.05 {
		frac = 0.05
	} else if frac > 1 {
		frac = 1
	}
	rvol := 0.0
	if avgVol > 0 {
		rvol = todayVol / (avgVol * frac)
	}

	// Conditions.
	green := cur.Close > cur.Open
	bounce := green && cur.Close > prev.Close
	oversold := r <= rsiOversold || (okBoll && cur.Low <= lower)
	belowVWAP := vwap > 0 && cur.Close < vwap
	belowOpen := dayOpen > 0 && cur.Close < dayOpen
	pulledBack := atr > 0 && (dayHigh-cur.Low) >= pullbackATRfrac*atr
	busy := rvol >= minRVOL

	if !(oversold && (belowVWAP || belowOpen) && pulledBack && busy && bounce) {
		return
	}
	if cdActive {
		return
	}

	knife := w.negativeNews(sym)
	w.send(formatAlert(sym, cur.Close, dayHigh, cur.Low, atr, r, vwap, dayOpen, rvol, knife))

	// Hand the dip to the quant pipeline (if wired) in a GOROUTINE — the hook makes a slow LLM
	// call (Agent 2), and running it inline would stall dip detection (and alerts) for every
	// other symbol in this scan pass. Its label is posted as a follow-up message, so a dip the
	// agent skips is still clearly labeled, never silently ignored.
	if w.hook != nil {
		sig := DipSignal{
			Symbol: sym, Time: now, Price: cur.Close, DayHigh: dayHigh, DipLow: cur.Low,
			ATR: atr, RSI: r, VWAP: vwap, DayOpen: dayOpen, RVOL: rvol,
			BounceVolume: cur.Volume, NegativeNews: knife,
		}
		go func() {
			if label := w.hook(sig); label != "" {
				w.send(sym + " — " + label)
			}
		}()
	}

	w.mu.Lock()
	w.cooldown[sym] = time.Now()
	w.mu.Unlock()
}

// sessionStats returns today's regular-session 1-min closes plus day high/open, session
// VWAP, and cumulative volume.
func (w *Watcher) sessionStats(sym string, sessionStart int64) (closes []float64, dayHigh, dayOpen, vwap, todayVol float64) {
	bars := w.engine.Snapshot(sym, 1)
	var pv float64
	first := true
	for _, b := range bars {
		if b.Time < sessionStart {
			continue
		}
		if first {
			dayOpen = b.Open
			first = false
		}
		if b.High > dayHigh {
			dayHigh = b.High
		}
		tp := (b.High + b.Low + b.Close) / 3
		pv += tp * b.Volume
		todayVol += b.Volume
		closes = append(closes, b.Close)
	}
	if todayVol > 0 {
		vwap = pv / todayVol
	}
	return
}

// negativeNews reports whether fresh (<24h) headlines for the symbol lean negative.
func (w *Watcher) negativeNews(sym string) bool {
	items, err := w.client.GetNews([]string{sym}, 5)
	if err != nil {
		return false
	}
	pos, neg := 0, 0
	for _, n := range items {
		if time.Since(n.CreatedAt) > 24*time.Hour {
			continue
		}
		switch n.Sentiment {
		case "negative":
			neg++
		case "positive":
			pos++
		}
	}
	return neg > 0 && neg > pos
}

func formatAlert(sym string, price, dayHigh, low, atr, r, vwap, dayOpen, rvol float64, knife bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔻➡️🟢 %s dip + bounce — $%.2f\n", sym, price)
	if atr > 0 {
		fmt.Fprintf(&b, "• pulled back $%.2f off the high (high $%.2f, ~%.1f×ATR)\n", dayHigh-low, dayHigh, (dayHigh-low)/atr)
	}
	fmt.Fprintf(&b, "• RSI %.0f", r)
	if vwap > 0 {
		fmt.Fprintf(&b, " · under VWAP $%.2f", vwap)
	}
	if dayOpen > 0 {
		fmt.Fprintf(&b, " · below open $%.2f", dayOpen)
	}
	fmt.Fprintf(&b, "\n• volume %.1f× normal\n", rvol)
	if knife {
		b.WriteString("• ⚠ fresh negative news — knife risk, check before buying")
	} else {
		b.WriteString("• ✅ no fresh bad news")
	}
	return b.String()
}

func (w *Watcher) send(text string) {
	body, _ := json.Marshal(map[string]string{"chat_id": w.chatID, "text": text})
	req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot"+w.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		log.Printf("dipwatch: telegram send error: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("dipwatch: telegram send status %s", resp.Status)
	}
}

// ---- indicator math ----

// rsi computes Wilder's RSI over the last `period` deltas of closes.
func rsi(closes []float64, period int) float64 {
	if len(closes) <= period {
		return 50
	}
	var gain, loss float64
	for i := len(closes) - period; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	if loss == 0 {
		return 100
	}
	rs := (gain / float64(period)) / (loss / float64(period))
	return 100 - 100/(1+rs)
}

// bollingerLower returns the lower Bollinger band (SMA(period) - mult*stdev) over the last
// `period` closes, and whether enough data existed.
func bollingerLower(closes []float64, period int, mult float64) (float64, bool) {
	if len(closes) < period {
		return 0, false
	}
	win := closes[len(closes)-period:]
	var sum float64
	for _, c := range win {
		sum += c
	}
	mean := sum / float64(period)
	var varSum float64
	for _, c := range win {
		varSum += (c - mean) * (c - mean)
	}
	sd := math.Sqrt(varSum / float64(period))
	return mean - mult*sd, true
}

// atr14 computes ATR(14) from daily highs/lows/closes.
func atr14(highs, lows, closes []float64) float64 {
	n := len(closes)
	if n < 2 {
		return 0
	}
	var trs []float64
	for i := 1; i < n; i++ {
		tr := highs[i] - lows[i]
		if x := math.Abs(highs[i] - closes[i-1]); x > tr {
			tr = x
		}
		if x := math.Abs(closes[i-1] - lows[i]); x > tr {
			tr = x
		}
		trs = append(trs, tr)
	}
	return avgLast(trs, 14)
}

func avgLast(vals []float64, n int) float64 {
	if len(vals) == 0 {
		return 0
	}
	if len(vals) > n {
		vals = vals[len(vals)-n:]
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func regularHours(et time.Time) bool {
	if et.Weekday() == time.Saturday || et.Weekday() == time.Sunday {
		return false
	}
	mins := et.Hour()*60 + et.Minute()
	return mins >= 9*60+30 && mins < 16*60
}
