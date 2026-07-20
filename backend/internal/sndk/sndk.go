package sndk

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/quant"
)

type Position struct {
	Symbol      string    `json:"symbol"`
	Direction   string    `json:"direction"`
	Qty         float64   `json:"qty"`
	EntryPrice  float64   `json:"entry_price"`
	OpenedAt    time.Time `json:"opened_at"`
	TargetPrice float64   `json:"target_price"`
	StopLoss    float64   `json:"stop_loss"`
	StopID      string    `json:"stop_id"`
	AgeMinutes  int       `json:"age_minutes"`
}

type Trade struct {
	Symbol     string    `json:"symbol"`
	Direction  string    `json:"direction"`
	Qty        float64   `json:"qty"`
	EntryPrice float64   `json:"entry_price"`
	ExitPrice  float64   `json:"exit_price"`
	PnL        float64   `json:"pnl"`
	Reason     string    `json:"reason"` // "target" | "stop_loss" | "catastrophic_stop" | "time_exit" | "safety_exit"
	OpenedAt   time.Time `json:"opened_at"`
	ClosedAt   time.Time `json:"closed_at"`
}

type Manager struct {
	mu             sync.RWMutex
	broker         *quant.Broker
	engine         *candles.Engine
	etz            *time.Location
	dataDir        string
	live           bool
	open           *Position
	trades         []Trade
	lastTickMinute int
	ensureLive     func(string)
}

type pythonSignal struct {
	Signal      bool    `json:"signal"`
	Probability float64 `json:"probability"`
	Close       float64 `json:"close"`
}

func New(broker *quant.Broker, engine *candles.Engine, etz *time.Location, dataDir string, live bool) *Manager {
	if etz == nil {
		etz = time.UTC
	}
	m := &Manager{
		broker:         broker,
		engine:         engine,
		etz:            etz,
		dataDir:        filepath.Join(dataDir, "sndk"),
		live:           live,
		trades:         []Trade{},
		lastTickMinute: -1,
	}
	_ = os.MkdirAll(m.dataDir, 0755)
	m.loadState()
	return m
}

func (m *Manager) SetEnsureLive(fn func(string)) {
	m.ensureLive = fn
}

func (m *Manager) Enabled() bool {
	return m != nil && m.broker.Enabled()
}

func (m *Manager) Start(ctx context.Context) {
	if !m.Enabled() {
		log.Printf("sndk: disabled (no PAPER_RBT keys)")
		return
	}

	// 1. Subscribe to SNDK quotes/candles to enable streaming price updates
	if m.ensureLive != nil {
		m.ensureLive("SNDK")
		log.Printf("sndk: subscribed to SNDK streaming quotes")
	}

	// 2. Launch tick loop (every 5 seconds for sub-minute exit monitoring)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.tick()
			}
		}
	}()
	log.Printf("sndk: started (live=%v, data_dir=%s)", m.live, m.dataDir)
}

func (m *Manager) tick() {
	now := time.Now().In(m.etz)
	mins := now.Hour()*60 + now.Minute()

	// Skip weekends
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return
	}

	// Active Position Exit Checks & Catastrophic Stop Monitor (9:30 AM to 4:00 PM ET)
	if mins >= 9*60+30 && mins <= 16*60 {
		m.monitorCatastrophicStops()
		m.manageStrategyExits(now)
		m.reconcileOrphans()
	}

	// Entry Checks (run on the closed 1m candle boundary)
	if mins >= 9*60+31 && mins < 15*60+50 {
		// Skip lunch hour doldrums (11:30 AM to 1:30 PM ET)
		if mins < 11*60+30 || mins > 13*60+30 {
			m.mu.Lock()
			currentMin := now.Minute()
			shouldRun := currentMin != m.lastTickMinute
			if shouldRun {
				m.lastTickMinute = currentMin
			}
			m.mu.Unlock()

			if shouldRun {
				m.runEntryScan(now)
			}
		}
	}
}

func (m *Manager) lastPrice() float64 {
	bars := m.engine.Snapshot("SNDK", 1)
	if len(bars) == 0 {
		return 0
	}
	return bars[len(bars)-1].Close
}

func (m *Manager) monitorCatastrophicStops() {
	m.mu.Lock()
	pos := m.open
	m.mu.Unlock()

	if pos == nil || pos.StopID == "" {
		return
	}

	fq, ap, status, err := m.broker.Order(pos.StopID)
	if err == nil && fq > 0 && status == "filled" {
		log.Printf("sndk: Alpaca protect CATASTROPHIC STOP hit for SNDK at $%.2f", ap)
		m.recordExit(ap, "catastrophic_stop")
		return
	}

	// Position vanished from the exchange without the stop filling (e.g. a manual
	// close-all): record the exit so local state can't track a ghost position forever.
	// Grace period avoids racing a just-filled entry.
	if time.Since(pos.OpenedAt) > 2*time.Minute {
		if q, qerr := m.broker.PositionQty("SNDK"); qerr == nil && q == 0 {
			px := m.lastPrice()
			if px <= 0 {
				px = pos.EntryPrice
			}
			log.Printf("sndk: SNDK no longer held on the exchange — recording exit")
			m.recordExit(px, "safety_exit")
		}
	}
}

// reconcileOrphans sweeps the account for SNDK shares the book doesn't know about and
// safely flattens them. Before this sweep existed, a phantom exit (book cleared while the
// sell never actually landed) stranded shares FOREVER: both exit monitors bail when the
// book is flat, so nothing ever looked at the account again, and the scalper kept buying
// 2 more on top — ~32 untracked, unprotected shares accumulated over 4 days in 2026-07.
// Only runs while the book is flat: when a position is tracked, the exit paths (which
// flatten the FULL account quantity) own the account.
func (m *Manager) reconcileOrphans() {
	m.mu.Lock()
	flat := m.open == nil
	m.mu.Unlock()
	if !flat {
		return
	}
	q, err := m.broker.PositionQty("SNDK")
	if err != nil || q <= 0 {
		return
	}
	log.Printf("sndk: RECONCILE found %.0f untracked SNDK share(s) on the account — flattening for safety", q)
	// Clear any stale resting orders first (an orphaned stop still holds the shares and
	// would block the sell with "insufficient qty available").
	_ = m.broker.CancelOpenOrders("SNDK")
	coid := fmt.Sprintf("sndk_orphan_exit_%d", time.Now().UnixNano())
	if _, serr := m.broker.MarketSell("SNDK", q, coid); serr != nil {
		log.Printf("sndk: orphan flatten failed (%v) — will retry next cycle", serr)
	}
}

func (m *Manager) manageStrategyExits(now time.Time) {
	m.mu.Lock()
	pos := m.open
	m.mu.Unlock()

	if pos == nil {
		return
	}

	price := m.lastPrice()
	if price <= 0 {
		return
	}

	// 1. Take Profit hit
	if price >= pos.TargetPrice {
		log.Printf("sndk: TAKE PROFIT hit at $%.2f (Target: $%.2f)", price, pos.TargetPrice)
		m.executeExit(price, "target")
		return
	}

	// 2. Stop Loss hit
	if price <= pos.StopLoss {
		log.Printf("sndk: STOP LOSS hit at $%.2f (Limit: $%.2f)", price, pos.StopLoss)
		m.executeExit(price, "stop_loss")
		return
	}

	// 3. Time Out (5 minutes hold)
	holdDuration := now.Sub(pos.OpenedAt)
	if holdDuration >= 5*time.Minute {
		log.Printf("sndk: 5-minute TIME OUT hit at $%.2f", price)
		m.executeExit(price, "time_exit")
		return
	}

	// 4. End-of-Day EOD Liquidation (15:59 ET)
	mins := now.Hour()*60 + now.Minute()
	if mins >= 15*60+59 {
		log.Printf("sndk: EOD LIQUIDATION hit at $%.2f", price)
		m.executeExit(price, "safety_exit")
		return
	}
}

func (m *Manager) executeExit(price float64, reason string) {
	m.mu.Lock()
	pos := m.open
	m.mu.Unlock()

	if pos == nil {
		return
	}

	log.Printf("sndk: executing market EXIT for SNDK (reason: %s)", reason)

	// Confirm-cancel the exchange-side catastrophic stop first (Alpaca's cancel is
	// ASYNC): selling while the stop is still live risks a double sell into a short.
	// If the stop FILLED before the cancel landed, that exit already happened — record
	// it at its real fill price and stop.
	if pos.StopID != "" {
		_ = m.broker.Cancel(pos.StopID)
		confirmed := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fq, ap, st, err := m.broker.Order(pos.StopID); err == nil {
				if st == "canceled" || st == "expired" || st == "rejected" || st == "replaced" {
					confirmed = true
					break
				}
				if st == "filled" && fq > 0 {
					m.recordExit(ap, "catastrophic_stop")
					return
				}
			}
			time.Sleep(400 * time.Millisecond)
		}
		if !confirmed {
			log.Printf("sndk: WARN: stop %s not confirmed canceled — deferring exit to avoid a double sell", pos.StopID)
			return
		}
	}

	// Sell the FULL account quantity, not the book quantity: if they've drifted apart the
	// account is the truth (selling more than held is a 403 "not allowed to short" loop;
	// selling less leaves a remainder stranded).
	qty := pos.Qty
	if aq, qerr := m.broker.PositionQty("SNDK"); qerr == nil {
		if aq <= 0 {
			// Nothing held — the shares already left (a stop fill we missed or a manual
			// close). Record the exit; there is nothing to sell.
			log.Printf("sndk: SNDK no longer held on the account — recording exit, not selling")
			m.recordExit(price, reason)
			return
		}
		qty = aq
	}

	coid := fmt.Sprintf("sndk_exit_%d", time.Now().Unix())
	id, err := m.broker.MarketSell("SNDK", qty, coid)
	if err != nil {
		// The stop is already canceled and the sell failed — re-protect IMMEDIATELY so
		// the position is never left naked, then retry the exit on a later tick. (This
		// exact gap stranded untracked ghost shares in 2026-07.)
		log.Printf("sndk: ERROR placing exit order: %v — re-placing protective stop", err)
		sc := fmt.Sprintf("sndk_stop_%d", time.Now().Unix())
		if sid, serr := m.broker.StopSell("SNDK", qty, pos.StopLoss, sc); serr == nil {
			m.mu.Lock()
			if m.open != nil {
				m.open.StopID = sid
			}
			m.mu.Unlock()
			m.saveState()
		} else {
			log.Printf("sndk: CRITICAL: SNDK held with NO stop and re-protect failed: %v", serr)
		}
		return
	}

	// Record the ACTUAL fill price, not the trigger price, so P&L matches the account.
	if _, ap := m.awaitFill(id, 12*time.Second); ap > 0 {
		price = ap
	}

	// PHANTOM-EXIT GUARD: confirm the shares actually LEFT the account before clearing the
	// book. Going flat on the book while the account still holds shares is the exact bug
	// that stranded ~32 untracked, unprotected shares over 4 days in 2026-07 — once the
	// book is flat, the entry scan happily buys on top of the pile.
	if q, qerr := m.broker.PositionQty("SNDK"); qerr != nil || q > 0 {
		if qerr != nil {
			log.Printf("sndk: exit fill unverifiable (%v) — keeping the book open, retrying next tick", qerr)
			return
		}
		log.Printf("sndk: exit incomplete — %.0f share(s) still held; re-protecting and retrying next tick", q)
		sc := fmt.Sprintf("sndk_stop_%d", time.Now().Unix())
		sid, serr := m.broker.StopSell("SNDK", q, pos.StopLoss, sc)
		if serr != nil {
			log.Printf("sndk: CRITICAL: SNDK remainder held with NO stop, re-protect failed: %v", serr)
			sid = ""
		}
		m.mu.Lock()
		if m.open != nil {
			m.open.Qty = q
			m.open.StopID = sid
		}
		m.mu.Unlock()
		m.saveState()
		return
	}
	m.recordExit(price, reason)
}

// awaitTerminal polls an order until it reaches a TERMINAL state, returning the last-seen
// filled qty/avg price and whether a terminal state was observed. It does not bail on the
// first partial fill — the rest of a market order usually lands moments later.
func (m *Manager) awaitTerminal(id string, max time.Duration) (float64, float64, bool) {
	deadline := time.Now().Add(max)
	var fq, ap float64
	for time.Now().Before(deadline) {
		q, p, status, err := m.broker.Order(id)
		if err == nil {
			if q > 0 {
				fq, ap = q, p
			}
			switch status {
			case "filled", "canceled", "expired", "rejected", "replaced", "done_for_day":
				return fq, ap, true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fq, ap, false
}

func (m *Manager) awaitFill(id string, max time.Duration) (float64, float64) {
	fq, ap, _ := m.awaitTerminal(id, max)
	return fq, ap
}

// settleEntry resolves an entry buy to its FINAL filled quantity; a still-live order at the
// deadline is canceled and re-read, so the book only ever records what actually filled.
func (m *Manager) settleEntry(id string) (float64, float64) {
	fq, ap, terminal := m.awaitTerminal(id, 12*time.Second)
	if terminal {
		return fq, ap
	}
	_ = m.broker.Cancel(id)
	fq, ap, _ = m.awaitTerminal(id, 4*time.Second)
	return fq, ap
}

func (m *Manager) recordExit(price float64, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.open == nil {
		return
	}

	pos := m.open
	pnl := (price - pos.EntryPrice) * pos.Qty

	trade := Trade{
		Symbol:     "SNDK",
		Direction:  pos.Direction,
		Qty:        pos.Qty,
		EntryPrice: pos.EntryPrice,
		ExitPrice:  price,
		PnL:        pnl,
		Reason:     reason,
		OpenedAt:   pos.OpenedAt,
		ClosedAt:   time.Now(),
	}

	m.trades = append(m.trades, trade)
	m.open = nil
	m.saveState()

	log.Printf("sndk: EXIT COMPLETE: SNDK, Qty: %.0f, Entry: $%.2f, Exit: $%.2f, PnL: $%.2f (%s)",
		trade.Qty, trade.EntryPrice, trade.ExitPrice, trade.PnL, trade.Reason)
}

func (m *Manager) runEntryScan(now time.Time) {
	m.mu.Lock()
	hasPosition := m.open != nil
	m.mu.Unlock()

	if hasPosition {
		return
	}

	bars := m.engine.Snapshot("SNDK", 1)
	if len(bars) < 100 {
		log.Printf("sndk: entry scan skipped (insufficient bars: %d)", len(bars))
		return
	}

	// Truncate to last 500 bars for indicator convergence (EMA memory)
	if len(bars) > 500 {
		bars = bars[len(bars)-500:]
	}

	// Write bars to recent_bars.json
	barsBytes, _ := json.Marshal(bars)
	barsFile := filepath.Join(m.dataDir, "recent_bars.json")
	_ = os.WriteFile(barsFile, barsBytes, 0644)

	pyPath := filepath.Join("..", "ml", ".venv", "Scripts", "python.exe")
	scriptPath := filepath.Join("..", "ml", "sndk_live_signals.py")

	sigFile := filepath.Join(m.dataDir, "signal.json")
	_ = os.Remove(sigFile) // remove stale signal file from previous tick

	cmd := exec.Command(pyPath, scriptPath, "--outdir", m.dataDir, "--recent-bars", barsFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("sndk: ERROR running sndk_live_signals.py: %v | Output: %s", err, string(output))
		return
	}

	b, err := os.ReadFile(sigFile)
	if err != nil {
		log.Printf("sndk: signal file not written: %v", err)
		return
	}

	var sig pythonSignal
	if err := json.Unmarshal(b, &sig); err != nil {
		log.Printf("sndk: failed to parse signal JSON: %v", err)
		return
	}

	if !sig.Signal {
		return
	}

	// Buy 2 shares of SNDK (hard gate)
	price := sig.Close
	qty := 2.0

	// Check available capital / safety margin
	acc, err := m.broker.Account()
	if err != nil {
		log.Printf("sndk: ERROR fetching paper account details: %v", err)
		return
	}

	buyingPower := acc.BuyingPower
	if buyingPower < price*qty {
		qty = math.Floor(buyingPower / price)
		if qty <= 0 {
			log.Printf("sndk: insufficient buying power (BP: $%.2f, price: $%.2f)", buyingPower, price)
			return
		}
	}

	log.Printf("sndk: ENTRY signal BUY SNDK (Conf: %.1f%%, price: $%.2f)", sig.Probability*100, price)

	entryCoid := fmt.Sprintf("sndk_entry_%d", time.Now().Unix())
	entryID, err := m.broker.MarketBuy("SNDK", qty, entryCoid)
	if err != nil {
		log.Printf("sndk: ERROR placing entry order: %v", err)
		return
	}

	// Settle the buy to a TERMINAL state and book ONLY what actually filled (a still-live
	// order at the deadline is canceled). Booking the intended qty on an unconfirmed fill
	// is how the book and the account drift apart.
	fillQty, fillPx := m.settleEntry(entryID)
	if fillQty < 1 {
		log.Printf("sndk: entry did not fill in time — canceled, nothing booked")
		return
	}
	qty = fillQty
	if fillPx > 0 {
		price = fillPx
	}

	// Set static targets / stops (±$8 around the actual entry)
	tpPrice := price + 8.00
	slPrice := price - 8.00

	// Place catastrophic stop sell on exchange
	stopCoid := fmt.Sprintf("sndk_stop_%d", time.Now().Unix())
	stopID, stopErr := m.broker.StopSell("SNDK", qty, slPrice, stopCoid)
	if stopErr != nil {
		log.Printf("sndk: ERROR placing catastrophic stop: %v. Cancelling position for safety.", stopErr)
		exitCoid := fmt.Sprintf("sndk_safety_exit_%d", time.Now().Unix())
		_, _ = m.broker.MarketSell("SNDK", qty, exitCoid)
		return
	}

	m.mu.Lock()
	m.open = &Position{
		Symbol:      "SNDK",
		Direction:   "Long",
		Qty:         qty,
		EntryPrice:  price,
		OpenedAt:    time.Now(),
		TargetPrice: tpPrice,
		StopLoss:    slPrice,
		StopID:      stopID,
		AgeMinutes:  0,
	}
	m.saveState()
	m.mu.Unlock()

	log.Printf("sndk: ENTRY COMPLETE for SNDK @ $%.2f (TP: $%.2f, SL: $%.2f, StopID: %s)",
		price, tpPrice, slPrice, stopID)
}

func (m *Manager) Report() interface{} {
	// Fetch Account details OUTSIDE the lock to prevent blocking the tick loop
	acc, err := m.broker.Account()
	cash := 100000.0
	equity := 100000.0
	if err == nil {
		cash = acc.Cash
		equity = acc.Equity
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var realizedPnL float64
	var totalTrades int
	var wins int

	for _, t := range m.trades {
		realizedPnL += t.PnL
		totalTrades++
		if t.PnL > 0 {
			wins++
		}
	}

	winRate := 0.0
	if totalTrades > 0 {
		winRate = (float64(wins) / float64(totalTrades)) * 100.0
	}

	// Fetch current position metrics
	var openPositions []Position
	var unrealizedPnL float64
	if m.open != nil {
		openPositions = append(openPositions, *m.open)
		currentPrice := m.lastPrice()
		if currentPrice > 0 {
			unrealizedPnL = (currentPrice - m.open.EntryPrice) * m.open.Qty
		}
	}

	return map[string]interface{}{
		"live":           true,
		"realized_pnl":   realizedPnL,
		"unrealized_pnl": unrealizedPnL,
		"total_trades":   totalTrades,
		"win_rate":       winRate,
		"cash":           cash,
		"equity":         equity,
		"open_count":     len(openPositions),
		"max_slots":      1,
		"positions":      openPositions,
		"trades":         m.trades,
	}
}

func (m *Manager) loadState() {
	p := filepath.Join(m.dataDir, "state.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var state struct {
		Open   *Position `json:"open"`
		Trades []Trade   `json:"trades"`
	}
	if err := json.Unmarshal(b, &state); err == nil {
		m.open = state.Open
		m.trades = state.Trades
	}
}

func (m *Manager) saveState() {
	p := filepath.Join(m.dataDir, "state.json")
	state := struct {
		Open   *Position `json:"open"`
		Trades []Trade   `json:"trades"`
	}{
		Open:   m.open,
		Trades: m.trades,
	}
	b, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(p, b, 0644)
}
