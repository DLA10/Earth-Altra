package signals

import (
	"os"
	"strings"
)

// Trend-alignment playbook — WHICH strategy may trade in WHICH (market trend, stock
// trend) cell. Both trends use the same rolling rule: yesterday's close vs the simple
// moving average of the prior 20 daily closes (the exact rule the Strategist's market
// digest and the backtester's regime brake already use). Seeded once at boot from the
// same daily-bar fetch that seeds ATR/avg-volume.
//
// The table is the 12-month regime × strategy study (2026-07-09; 22k counterfactual
// outcomes, backend/data/ml_dataset_12mo.jsonl): long strategies on this universe earn
// when they trade WITH alignment — mean-reversion (vwap_reclaim +0.121 mean R,
// dip_bounce +0.052) only when stock AND market are both in downtrends; momentum_cont
// (+0.053) only when both are in uptrends; rel_strength (+0.227) only in its literal
// definition — a stock holding up while the market falls. The most toxic cell in the
// whole study was buying a stock in its own downtrend during a market uptrend (−228R
// across all strategies): a lone weak stock in a strong tape is weak for a reason.
// orb_breakout was positive in all four cells and is unrestricted. fh_reversal was
// negative everywhere and is retired from trading (it keeps journaling counterfactuals,
// so the eval scoreboard can argue for its return with data).
//
// Unknown strategies and unknown trends fail OPEN (allowed) — the gate only acts on
// evidence it actually has.
//
// THROUGHPUT MODE (2026-07-16, see THROUGHPUT_MODE.md): the strict best-cell-only table
// below produced near-zero live trades (on a red-QQQ day nearly every strategy is off).
// The default now blocks only the PROVEN-NEGATIVE evidence: the −228R toxic cell
// (mkt-up/sym-down, all strategies except orb_breakout) and fh_reversal (negative in
// every cell). Everything else trades and journals, so the scoreboard can re-argue the
// strict table with fresh data. Set QUANT_ALIGN_STRICT=true to restore the original
// best-cell-only playbook exactly as validated.

// AlignmentAllowed reports whether a strategy's playbook permits trading in the given
// (market up?, stock up?) trend cell.
func AlignmentAllowed(strategy string, mktUp, symUp bool) bool {
	if strictAlign {
		return alignmentStrict(strategy, mktUp, symUp)
	}
	if strategy == "fh_reversal" {
		return false // retired: negative in every cell; journals only
	}
	if mktUp && !symUp && strategy != "orb_breakout" {
		return false // the −228R toxic cell: a lone weak stock in a strong tape
	}
	return true
}

// alignmentStrict is the original validated best-cell-only table (pre-2026-07-16).
func alignmentStrict(strategy string, mktUp, symUp bool) bool {
	switch strategy {
	case "vwap_reclaim":
		return mktUp == symUp // aligned tapes only (both up or both washed out)
	case "momentum_cont":
		return mktUp && symUp // momentum needs the tide AND the boat rising
	case "dip_bounce":
		return !mktUp && !symUp // buy dips only when everything is washed out together
	case "rel_strength":
		return !mktUp && symUp // the literal definition: strong stock, weak tape
	case "orb_breakout":
		return true // positive in all four cells
	case "fh_reversal":
		return false // retired: negative in every cell; journals only
	}
	return true
}

// strictAlign restores the original playbook when QUANT_ALIGN_STRICT=true.
var strictAlign = strings.EqualFold(strings.TrimSpace(os.Getenv("QUANT_ALIGN_STRICT")), "true")

// trendCell renders the cell for journals/skip notes, e.g. "mkt-down/sym-down".
func trendCell(mktUp, symUp bool) string {
	c := "mkt-down"
	if mktUp {
		c = "mkt-up"
	}
	if symUp {
		return c + "/sym-up"
	}
	return c + "/sym-down"
}
