package rbt

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/quant"
)

// probMin is the ML-probability floor for taking a signal. Throughput mode 2026-07-16:
// default lowered 0.65 → 0.60 (original 0.65 — set RBT_PROB_MIN=0.65 to roll back).
var probMin = func() float64 {
	if v := strings.TrimSpace(os.Getenv("RBT_PROB_MIN")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0.60
}()

// RbtUniverse holds the 100 co-moving tickers monitored by RBT.
var RbtUniverse = []string{
	"ADI", "AMD", "AMAT", "ASML", "AVGO", "INTC", "KLAC", "LRCX", "MCHP", "MPWR",
	"MRVL", "MU", "NVDA", "NXPI", "ON", "QCOM", "SMCI", "TSM", "TXN", "ARM",
	"COP", "CVX", "EOG", "MPC", "OXY", "PSX", "SLB", "VLO", "WMB", "XOM",
	"HAL", "BKR", "AR", "DVN", "FANG", "KMI", "OKE", "APA", "LNG", "EQT",
	"AAPL", "ACN", "ADBE", "AMZN", "ANET", "CRM", "CSCO", "GOOGL", "IBM", "INTU",
	"META", "MSFT", "NFLX", "NOW", "ORCL", "PLTR", "SHOP", "SNOW", "UBER", "DELL",
	"JPM", "BAC", "MS", "GS", "C", "WFC", "BK", "SCHW", "COF", "USB",
	"AXP", "BLK", "MET", "PRU", "PNC", "TFC", "FITB", "KEY", "RF", "HBAN",
	"FCX", "NEM", "NUE", "AA", "ALB", "CLF", "STLD", "MLM", "VMC", "APD",
	"CAT", "DE", "HON", "EMR", "ETN", "GE", "ITW", "PH", "ROK", "PWR",
}

// Position holds one active RBT mean-reversion trade.
type Position struct {
	Symbol      string    `json:"symbol"`
	Direction   string    `json:"direction"` // "Long" | "Short"
	Qty         float64   `json:"qty"`
	EntryPrice  float64   `json:"entry_price"`
	OpenedAt    time.Time `json:"opened_at"`
	TargetPrice float64   `json:"target_price"`
	StopLoss    float64   `json:"stop_loss"`
	StopID      string    `json:"stop_id"` // exchange-side catastrophic stop order ID
	Age         int       `json:"age"`     // hold age in trading sessions
}

// Trade holds one closed RBT trade record.
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

// DaySnap is one symbol's session-so-far OHLCV aggregate, fetched via REST at scan time.
// The 200-plan design: the desk scans once a day (15:50 ET), so the scan universe does NOT
// need all-day trade/quote streaming — a REST snapshot decouples universe size from the
// load on the single SIP connection the live Execution page depends on.
type DaySnap struct {
	Close  float64
	High   float64
	Low    float64
	Volume float64
}

// Manager runs the entire RBT (Rubber Band Trading) paper-trading desk.
type Manager struct {
	mu         sync.RWMutex
	broker     *quant.Broker
	engine     *candles.Engine
	etz        *time.Location
	dataDir    string
	live       bool
	universe   []string // scan universe (200 plan: curated liquid set ∪ legacy 100)
	open       map[string]*Position
	trades     []Trade
	dayKey     string
	entryRun   bool
	ageRun     bool
	ensureLive func(string)
	daySnap    func([]string) (map[string]DaySnap, error) // REST session-OHLCV fetch (nil = engine fallback)
}

type pythonSignal struct {
	Ticker      string  `json:"ticker"`
	Direction   string  `json:"direction"`
	Probability float64 `json:"probability"`
	Close       float64 `json:"close"`
	ZVal        float64 `json:"z_val"`
	Target      float64 `json:"target"`
	StopLoss    float64 `json:"stop_loss"`
}

// New builds an RBT Manager. universe is the scan set (empty = the legacy RbtUniverse).
func New(broker *quant.Broker, engine *candles.Engine, etz *time.Location, dataDir string, live bool, universe []string) *Manager {
	if etz == nil {
		etz = time.UTC
	}
	if len(universe) == 0 {
		universe = RbtUniverse
	}
	m := &Manager{
		broker:   broker,
		engine:   engine,
		etz:      etz,
		dataDir:  filepath.Join(dataDir, "rbt"),
		live:     live,
		universe: universe,
		open:     map[string]*Position{},
		trades:   []Trade{},
	}
	_ = os.MkdirAll(m.dataDir, 0755)
	m.loadState()
	return m
}

// SetEnsureLive registers the symbol streaming activation callback.
func (m *Manager) SetEnsureLive(fn func(string)) {
	m.ensureLive = fn
}

// SetDaySnapFn registers the REST session-OHLCV fetcher used at scan time.
func (m *Manager) SetDaySnapFn(fn func([]string) (map[string]DaySnap, error)) {
	m.daySnap = fn
}

// Enabled returns true if RBT has paper broker keys.
func (m *Manager) Enabled() bool {
	return m != nil && m.broker.Enabled()
}

// Start launches the RBT ticks.
func (m *Manager) Start(ctx context.Context) {
	if !m.Enabled() {
		log.Printf("rbt: disabled (no PAPER_RBT_KEY/SECRET)")
		return
	}

	// 1. Stream only HELD positions (exit monitoring marks to live 1-min candles). The scan
	// universe itself is priced by a REST snapshot at 15:50 (SetDaySnapFn), so widening the
	// universe adds zero load to the single SIP stream the Execution page depends on.
	if m.ensureLive != nil {
		m.mu.RLock()
		held := make([]string, 0, len(m.open))
		for sym := range m.open {
			held = append(held, sym)
		}
		m.mu.RUnlock()
		for _, sym := range held {
			m.ensureLive(sym)
		}
		log.Printf("rbt: streaming %d held position(s); scan universe %d symbols priced via REST at scan time",
			len(held), len(m.universe))
	}

	// 2. Launch nightly retraining scheduler
	m.runNightlyRetrain(ctx)

	// 3. Launch tick loop
	go func() {
		ticker := time.NewTicker(15 * time.Second)
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
	log.Printf("rbt: started (live=%v, data_dir=%s)", m.live, m.dataDir)
}

// tick checks exits and runs entry scans.
func (m *Manager) tick() {
	now := time.Now().In(m.etz)
	day := now.Format("2006-01-02")

	m.mu.Lock()
	if day != m.dayKey {
		m.dayKey = day
		m.entryRun = false
		m.ageRun = false
	}
	m.mu.Unlock()

	// Skip weekends
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return
	}

	mins := now.Hour()*60 + now.Minute()

	// 1. Exit & entry check (runs at 15:50 ET, matching the daily bar validation close)
	if mins >= 15*60+50 && mins < 16*60 {
		m.mu.Lock()
		alreadyRun := m.entryRun
		m.mu.Unlock()
		if !alreadyRun {
			// Manage strategy exits and find new entries on the forming close
			m.manageStrategyExits(now)
			m.runEntryScan(now)
		}
	}

	// 2. Catastrophic Stop Loss monitor (checks Alpaca order fills constantly during session)
	if mins >= 9*60+30 && mins <= 16*60 {
		m.monitorCatastrophicStops()
	}

	// 3. EOD Hold-Age check + Time Exits (runs at 15:55 ET inside regular market hours)
	if mins >= 15*60+55 && mins < 15*60+59 {
		m.mu.Lock()
		alreadyAge := m.ageRun
		m.mu.Unlock()
		if !alreadyAge {
			m.runEodRollover(now)
		}
	}
}

func (m *Manager) lastPrice(sym string) float64 {
	bars := m.engine.Snapshot(sym, 1)
	if len(bars) == 0 {
		return 0
	}
	return bars[len(bars)-1].Close
}

// monitorCatastrophicStops checks if any exchange-side protective GTC stop order has filled.
func (m *Manager) monitorCatastrophicStops() {
	m.mu.Lock()
	symbols := make([]string, 0, len(m.open))
	for sym := range m.open {
		symbols = append(symbols, sym)
	}
	m.mu.Unlock()

	for _, sym := range symbols {
		m.mu.Lock()
		pos, ok := m.open[sym]
		m.mu.Unlock()
		if !ok || pos.StopID == "" {
			continue
		}

		// Await/check order status on Alpaca
		fq, ap, status, err := m.broker.Order(pos.StopID)
		if err == nil && fq > 0 && status == "filled" {
			log.Printf("rbt: exchange-side CATASTROPHIC STOP hit for %s at $%.2f", sym, ap)
			m.recordExit(sym, ap, "catastrophic_stop")
			continue
		}

		// Position vanished from the exchange without the stop filling (e.g. a manual
		// close-all liquidation): record the exit so local state can't track a ghost
		// position forever. Grace period avoids racing a just-filled entry.
		if m.live && time.Since(pos.OpenedAt) > 2*time.Minute {
			if q, qerr := m.broker.PositionQty(sym); qerr == nil && q == 0 {
				px := m.lastPrice(sym)
				if px <= 0 {
					px = pos.EntryPrice
				}
				log.Printf("rbt: %s no longer held on the exchange — recording exit", sym)
				m.recordExit(sym, px, "safety_exit")
			}
		}
	}
}

// manageStrategyExits evaluates stop losses (1.5x ATR) and target means at 15:50 ET on the forming close.
func (m *Manager) manageStrategyExits(now time.Time) {
	m.mu.Lock()
	symbols := make([]string, 0, len(m.open))
	for sym := range m.open {
		symbols = append(symbols, sym)
	}
	m.mu.Unlock()

	for _, sym := range symbols {
		m.mu.Lock()
		pos, ok := m.open[sym]
		m.mu.Unlock()
		if !ok {
			continue
		}

		price := m.lastPrice(sym)
		if price <= 0 {
			continue
		}

		exit := false
		reason := ""

		if pos.Direction == "Long" {
			if price >= pos.TargetPrice {
				exit = true
				reason = "target"
			} else if price <= pos.StopLoss {
				exit = true
				reason = "stop_loss"
			}
		} else { // Short
			if price <= pos.TargetPrice {
				exit = true
				reason = "target"
			} else if price >= pos.StopLoss {
				exit = true
				reason = "stop_loss"
			}
		}

		if exit {
			m.executeMarketExit(sym, reason)
		}
	}
}

func (m *Manager) executeMarketExit(sym string, reason string) {
	m.mu.Lock()
	pos, ok := m.open[sym]
	m.mu.Unlock()
	if !ok {
		return
	}

	log.Printf("rbt: executing market EXIT for %s (reason: %s)", sym, reason)

	// Cancel exchange-side catastrophic stop first and confirm status (Bug 2 race protection)
	if pos.StopID != "" && m.live {
		_ = m.broker.Cancel(pos.StopID)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, st, err := m.broker.Order(pos.StopID); err == nil {
				if st == "canceled" || st == "expired" || st == "rejected" || st == "replaced" {
					break
				}
				if st == "filled" { // stop filled beat our cancellation
					exitPrice := m.lastPrice(sym)
					if exitPrice <= 0 {
						exitPrice = pos.EntryPrice
					}
					m.recordExit(sym, exitPrice, "catastrophic_stop")
					return
				}
			}
			time.Sleep(400 * time.Millisecond)
		}
	}

	exitPrice := pos.EntryPrice // fallback
	qty := pos.Qty
	if m.live {
		actualQty, err := m.broker.PositionQty(sym)
		if err != nil {
			// Transient broker error: do NOT record an exit (that would strand real
			// shares as untracked ghosts). Keep the position; a later pass retries and
			// the exchange-side stop still protects it.
			log.Printf("rbt: exit for %s deferred: position check failed: %v", sym, err)
			return
		}
		if math.Abs(actualQty) == 0 {
			log.Printf("rbt: position %s already flat on exchange. Recording exit.", sym)
			exitPrice = m.lastPrice(sym)
			if exitPrice <= 0 {
				exitPrice = pos.EntryPrice
			}
			m.recordExit(sym, exitPrice, "catastrophic_stop")
			return
		}
		qty = math.Abs(actualQty) // safety: exit real exchange quantity
		if qty > pos.Qty {
			// Shared-account guard: another desk may hold the same symbol — only ever
			// exit THIS desk's shares, never the account total.
			qty = pos.Qty
		}

		coid := fmt.Sprintf("rbt_exit_%s_%d", sym, time.Now().Unix())
		var id string
		if pos.Direction == "Long" {
			id, err = m.broker.MarketSell(sym, qty, coid)
		} else {
			id, err = m.broker.MarketBuy(sym, qty, coid)
		}
		if err != nil {
			log.Printf("rbt: ERROR placing exit order for %s: %v", sym, err)
			return
		}
		// Await the actual fill price to prevent P&L drift (Bug 5)
		_, fillPrice := m.awaitFill(id, 12*time.Second)
		if fillPrice > 0 {
			exitPrice = fillPrice
		} else {
			exitPrice = m.lastPrice(sym)
		}
	} else {
		exitPrice = m.lastPrice(sym)
	}

	m.recordExit(sym, exitPrice, reason)
}

func (m *Manager) recordExit(sym string, exitPrice float64, reason string) {
	m.mu.Lock()
	pos, ok := m.open[sym]
	if !ok {
		m.mu.Unlock()
		return
	}

	var pnl float64
	if pos.Direction == "Long" {
		pnl = (exitPrice - pos.EntryPrice) * pos.Qty
	} else {
		pnl = (pos.EntryPrice - exitPrice) * pos.Qty
	}

	trade := Trade{
		Symbol:     sym,
		Direction:  pos.Direction,
		Qty:        pos.Qty,
		EntryPrice: pos.EntryPrice,
		ExitPrice:  exitPrice,
		PnL:        pnl,
		Reason:     reason,
		OpenedAt:   pos.OpenedAt,
		ClosedAt:   time.Now(),
	}

	delete(m.open, sym)
	m.trades = append(m.trades, trade)
	m.mu.Unlock()

	m.saveState()
	log.Printf("rbt: EXIT COMPLETE %s %s, Qty: %.0f, Entry: $%.2f, Exit: $%.2f, PnL: $%.2f (%s)",
		pos.Direction, sym, pos.Qty, pos.EntryPrice, exitPrice, pnl, reason)
}

func (m *Manager) runEntryScan(now time.Time) {
	m.mu.Lock()
	m.entryRun = true
	m.mu.Unlock()

	log.Printf("rbt: launching non-blocking entry scan (15:50 ET)...")

	// Run in a separate goroutine so we don't block exits during the Python run (Bug 9)
	go func() {
		// Aggregate today's 1-minute bars into a daily OHLCV bar (Rel_Vol / ATR bug fix)
		nyc, _ := time.LoadLocation("America/New_York")
		nowNYC := time.Now().In(nyc)
		marketOpenTime := time.Date(nowNYC.Year(), nowNYC.Month(), nowNYC.Day(), 9, 30, 0, 0, nyc)
		marketOpenUnix := marketOpenTime.Unix()

		livePrices := make(map[string]map[string]float64)
		// Primary: one REST multi-symbol fetch of today's session bars (works for ANY
		// universe size, no streaming needed). Fallback per symbol: the candle engine
		// (held names stream; anything else the engine happens to track).
		var snaps map[string]DaySnap
		if m.daySnap != nil {
			var serr error
			if snaps, serr = m.daySnap(m.universe); serr != nil {
				log.Printf("rbt: REST session snapshot failed (%v) — engine fallback only", serr)
			}
		}
		for _, sym := range m.universe {
			if s, ok := snaps[sym]; ok && s.Close > 0 {
				livePrices[sym] = map[string]float64{
					"close": s.Close, "high": s.High, "low": s.Low, "volume": s.Volume,
				}
				continue
			}
			bars := m.engine.Snapshot(sym, 1)
			if len(bars) > 0 {
				todayClose := 0.0
				todayHigh := -math.MaxFloat64
				todayLow := math.MaxFloat64
				todayVolume := 0.0
				hasAny := false

				for _, c := range bars {
					if c.Time >= marketOpenUnix {
						hasAny = true
						todayClose = c.Close
						if c.High > todayHigh {
							todayHigh = c.High
						}
						if c.Low < todayLow {
							todayLow = c.Low
						}
						todayVolume += c.Volume
					}
				}

				if hasAny {
					livePrices[sym] = map[string]float64{
						"close":  todayClose,
						"high":   todayHigh,
						"low":    todayLow,
						"volume": todayVolume,
					}
				} else {
					// Fallback to the very last available minute bar if no today's session bars exist yet
					lastBar := bars[len(bars)-1]
					livePrices[sym] = map[string]float64{
						"close":  lastBar.Close,
						"high":   lastBar.High,
						"low":    lastBar.Low,
						"volume": lastBar.Volume * 390.0, // scale volume as a daily estimate
					}
				}
			}
		}

		priceBytes, _ := json.Marshal(livePrices)
		priceFile := filepath.Join(m.dataDir, "live_prices.json")
		_ = os.WriteFile(priceFile, priceBytes, 0644)

		pyPath := filepath.Join("..", "ml", ".venv", "Scripts", "python.exe")
		scriptPath := filepath.Join("..", "ml", "rbt_live_signals.py")

		cmd := exec.Command(pyPath, scriptPath, "--outdir", m.dataDir, "--live-prices", priceFile)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("rbt: ERROR running rbt_live_signals.py: %v | Output: %s", err, string(output))
			return
		}

		sigFile := filepath.Join(m.dataDir, "signals_today.json")
		b, err := os.ReadFile(sigFile)
		if err != nil {
			log.Printf("rbt: no signals file written: %v", err)
			return
		}

		var sigs []pythonSignal
		if err := json.Unmarshal(b, &sigs); err != nil {
			log.Printf("rbt: failed to parse signals JSON: %v", err)
			return
		}

		sort.Slice(sigs, func(i, j int) bool {
			return sigs[i].Probability > sigs[j].Probability
		})

		acc, err := m.broker.Account()
		if err != nil {
			log.Printf("rbt: ERROR fetching paper account details: %v", err)
			return
		}

		m.mu.Lock()
		openCount := len(m.open)
		m.mu.Unlock()

		maxSlots := 5
		slotsLeft := maxSlots - openCount
		if slotsLeft <= 0 {
			log.Printf("rbt: portfolio is full. Skipping new entries.")
			return
		}

		// Equal allocation capped at Available Buying Power with a safety margin (Bug 6)
		safetyBP := acc.BuyingPower * 0.90
		tradeBudget := safetyBP / float64(slotsLeft)
		equityBudget := acc.Equity / float64(maxSlots)
		if tradeBudget > equityBudget {
			tradeBudget = equityBudget // cap at normal slot value
		}

		for _, sig := range sigs {
			if slotsLeft <= 0 {
				break
			}

			m.mu.RLock()
			_, exists := m.open[sig.Ticker]
			m.mu.RUnlock()
			if exists {
				continue
			}

			if sig.Probability < probMin {
				continue
			}

			// Validate buying power safety
			price := m.lastPrice(sig.Ticker)
			if price <= 0 {
				price = sig.Close
			}

			qty := math.Floor(tradeBudget / price)
			if qty <= 0 {
				continue
			}

			log.Printf("rbt: ENTRY signal %s %s (Conf: %.1f%%, price: $%.2f)", sig.Direction, sig.Ticker, sig.Probability*100, price)

			var entryPrice float64
			var stopID string
			posQty := qty

			if m.live {
				coid := fmt.Sprintf("rbt_entry_%s_%d", sig.Ticker, time.Now().Unix())
				var id string
				if sig.Direction == "Long" {
					id, err = m.broker.MarketBuy(sig.Ticker, qty, coid)
				} else {
					log.Printf("rbt: WARNING: placing Short entry for %s. Ensure account is Margin and symbol is shortable.", sig.Ticker)
					id, err = m.broker.MarketSell(sig.Ticker, qty, coid)
				}
				if err != nil {
					log.Printf("rbt: ERROR placing entry order for %s: %v", sig.Ticker, err)
					continue
				}

				// Await the actual fill price AND filled quantity (Bug 5 + partial-fill stop sizing)
				filledQty, fillPrice := m.awaitFill(id, 12*time.Second)
				if fillPrice > 0 {
					entryPrice = fillPrice
				} else {
					entryPrice = price
				}
				if filledQty > 0 {
					posQty = filledQty // protect/track exactly the shares actually filled
				}

				// Place an exchange-side catastrophic stop at 2.5x ATR to protect overnight gaps (Bugs 3, 4)
				// The closer 1.5x ATR strategy exit is checked at the close to avoid noise wicks.
				diffPrice := math.Abs(sig.Close-sig.StopLoss) / 1.5 // 1.0x ATR
				catastrophicStop := sig.StopLoss
				stopCoid := fmt.Sprintf("rbt_stop_%s_%d", sig.Ticker, time.Now().Unix())
				var stopErr error
				if sig.Direction == "Long" {
					catastrophicStop = entryPrice - (2.5 * diffPrice)
					stopID, stopErr = m.broker.StopSell(sig.Ticker, posQty, catastrophicStop, stopCoid)
				} else {
					catastrophicStop = entryPrice + (2.5 * diffPrice)
					stopID, stopErr = m.broker.StopBuy(sig.Ticker, posQty, catastrophicStop, stopCoid)
				}

				if stopErr != nil {
					log.Printf("rbt: ERROR placing catastrophic stop for %s: %v. Cancelling position for safety.", sig.Ticker, stopErr)
					exitCoid := fmt.Sprintf("rbt_safety_exit_%s_%d", sig.Ticker, time.Now().Unix())
					var exitPrice float64
					var exitID string
					if sig.Direction == "Long" {
						exitID, _ = m.broker.MarketSell(sig.Ticker, posQty, exitCoid)
					} else {
						exitID, _ = m.broker.MarketBuy(sig.Ticker, posQty, exitCoid)
					}
					if exitID != "" {
						_, exitPrice = m.awaitFill(exitID, 5*time.Second)
					}
					if exitPrice <= 0 {
						exitPrice = entryPrice
					}

					pnl := (exitPrice - entryPrice) * posQty
					if sig.Direction == "Short" {
						pnl = (entryPrice - exitPrice) * posQty
					}

					m.mu.Lock()
					m.trades = append(m.trades, Trade{
						Symbol:     sig.Ticker,
						Direction:  sig.Direction,
						Qty:        posQty,
						EntryPrice: entryPrice,
						ExitPrice:  exitPrice,
						PnL:        pnl,
						Reason:     "safety_exit",
						OpenedAt:   time.Now(),
						ClosedAt:   time.Now(),
					})
					m.mu.Unlock()
					m.saveState()
					continue // skip adding to open positions
				}
			} else {
				entryPrice = price
			}

			newPos := &Position{
				Symbol:      sig.Ticker,
				Direction:   sig.Direction,
				Qty:         posQty,
				EntryPrice:  entryPrice,
				OpenedAt:    time.Now(),
				TargetPrice: sig.Target,
				StopLoss:    sig.StopLoss,
				StopID:      stopID,
				Age:         0,
			}

			m.mu.Lock()
			m.open[sig.Ticker] = newPos
			m.mu.Unlock()

			// The scan universe no longer streams (200 plan) — start streaming THIS name
			// now so exit monitoring can mark it to live 1-min candles until it closes.
			if m.ensureLive != nil {
				m.ensureLive(sig.Ticker)
			}

			slotsLeft--
			log.Printf("rbt: ENTRY COMPLETE for %s @ $%.2f (TP: $%.2f, Strategy SL: $%.2f, Rested StopID: %s)",
				sig.Ticker, entryPrice, sig.Target, sig.StopLoss, stopID)
		}

		m.saveState()
	}()
}

func (m *Manager) runEodRollover(now time.Time) {
	m.mu.Lock()
	m.ageRun = true
	m.mu.Unlock()

	log.Printf("rbt: running EOD rollover check (15:55 ET)...")

	m.mu.Lock()
	var toExit []string
	for _, pos := range m.open {
		// Only increment age if the position was opened before today (Bug 8 off-by-one check)
		if now.Sub(pos.OpenedAt) > 6*time.Hour {
			pos.Age++
			if pos.Age >= 5 {
				toExit = append(toExit, pos.Symbol)
			}
		}
	}
	m.mu.Unlock()

	for _, sym := range toExit {
		m.executeMarketExit(sym, "time_exit")
	}

	m.saveState()
}

func (m *Manager) runNightlyRetrain(ctx context.Context) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}

	run := func(reason string) {
		log.Printf("rbt-retrain: starting (%s)", reason)
		pyPath := filepath.Join("..", "ml", ".venv", "Scripts", "python.exe")
		trainScript := filepath.Join("..", "ml", "rbt_train.py")

		// 45 min: the 200-plan universe (~210 names) roughly quadruples the pairwise
		// cointegration sweep vs the old 100 — 15 min killed it mid-run.
		cctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
		defer cancel()

		cmd := exec.CommandContext(cctx, pyPath, trainScript, "--outdir", m.dataDir)
		cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
		out, err := cmd.CombinedOutput()
		tail := string(out)
		if len(tail) > 600 {
			tail = tail[len(tail)-600:]
		}
		if err != nil {
			log.Printf("rbt-retrain: failed: %v | %s", err, tail)
			return
		}
		log.Printf("rbt-retrain: done | %s", tail)
	}

	// Boot catch-up: retrain if the models are missing OR the cached history is stale (>24h).
	// The live scorer appends today's row onto history_closes.csv, so if a weekday 17:05 window
	// was ever missed (e.g. the machine was off after the close), the cache freezes and a
	// calendar gap forms that silently corrupts every rolling feature. A >24h check heals that
	// on the next boot; on an always-on machine the nightly run keeps the cache <24h old so this
	// never fires redundantly.
	modelCheck := filepath.Join(m.dataDir, "models", "lgbm_model.pkl")
	histCheck := filepath.Join(m.dataDir, "history_closes.csv")
	if _, err := os.Stat(modelCheck); os.IsNotExist(err) {
		go run("boot catch-up: models missing")
	} else if info, serr := os.Stat(histCheck); serr != nil || time.Since(info.ModTime()) > 24*time.Hour {
		go run("boot catch-up: cache stale (>24h)")
	}

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		lastDay := ""

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now().In(loc)
				day := now.Format("2006-01-02")
				weekday := now.Weekday() >= time.Monday && now.Weekday() <= time.Friday
				if !weekday || day == lastDay || now.Hour() != 17 || now.Minute() < 5 || now.Minute() > 20 {
					continue
				}
				lastDay = day
				go run("nightly window")
			}
		}
	}()
}

func (m *Manager) awaitFill(id string, max time.Duration) (float64, float64) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		fq, ap, status, err := m.broker.Order(id)
		if err == nil && fq > 0 && (status == "filled" || status == "partially_filled") {
			return fq, ap
		}
		time.Sleep(700 * time.Millisecond)
	}
	return 0, 0
}

// ReportState matches frontend serialization needs.
type ReportState struct {
	Live        bool        `json:"live"`
	RealizedPnL float64     `json:"realized_pnl"`
	Unrealized  float64     `json:"unrealized_pnl"`
	TotalTrades int         `json:"total_trades"`
	WinRate     float64     `json:"win_rate"`
	OpenCount   int         `json:"open_count"`
	MaxSlots    int         `json:"max_slots"`
	Cash        float64     `json:"cash"`
	Equity      float64     `json:"equity"`
	Positions   []*Position `json:"positions"`
	Trades      []Trade     `json:"trades"`
}

// Report compiles the status report for the frontend.
func (m *Manager) Report() ReportState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var realized float64
	wins := 0
	for _, t := range m.trades {
		realized += t.PnL
		if t.PnL > 0 {
			wins++
		}
	}

	winRate := 0.0
	if len(m.trades) > 0 {
		winRate = float64(wins) / float64(len(m.trades)) * 100
	}

	var openPos []*Position
	var unrealized float64
	for _, pos := range m.open {
		openPos = append(openPos, pos)
		price := m.lastPrice(pos.Symbol)
		if price > 0 {
			if pos.Direction == "Long" {
				unrealized += (price - pos.EntryPrice) * pos.Qty
			} else {
				unrealized += (pos.EntryPrice - price) * pos.Qty
			}
		}
	}

	cash := 100000.0
	equity := 100000.0 + realized + unrealized
	if m.Enabled() {
		acc, err := m.broker.Account()
		if err == nil {
			cash = acc.Cash
			equity = acc.Equity
		}
	}

	return ReportState{
		Live:        m.live,
		RealizedPnL: realized,
		Unrealized:  unrealized,
		TotalTrades: len(m.trades),
		WinRate:     winRate,
		OpenCount:   len(m.open),
		MaxSlots:    5,
		Cash:        cash,
		Equity:      equity,
		Positions:   openPos,
		Trades:      m.trades,
	}
}

type persistedState struct {
	Open   map[string]*Position `json:"open"`
	Trades []Trade              `json:"trades"`
}

func (m *Manager) loadState() {
	path := filepath.Join(m.dataDir, "state.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state persistedState
	if err := json.Unmarshal(b, &state); err != nil {
		log.Printf("rbt: failed to unmarshal state: %v", err)
		return
	}
	m.mu.Lock()
	if state.Open != nil {
		m.open = state.Open
	}
	if state.Trades != nil {
		m.trades = state.Trades
	}
	m.mu.Unlock()
}

func (m *Manager) saveState() {
	path := filepath.Join(m.dataDir, "state.json")
	m.mu.RLock()
	state := persistedState{Open: m.open, Trades: m.trades}
	m.mu.RUnlock()

	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("rbt: failed to marshal state: %v", err)
		return
	}
	_ = os.WriteFile(path, b, 0644)
}
