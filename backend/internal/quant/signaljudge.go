package quant

import (
	"context"
	"encoding/json"
	"strings"
)

// SignalJudge is the LLM ENTRY reviewer for the multi-strategy signal engine — the
// generalized sibling of the dip pipeline's Agent 2. The deterministic layers (strategy
// detector + time-of-day gate + risk rails) have already validated the setup mechanics;
// the judge is the red-flag layer: veto entries a mechanical rule can't see are bad
// (exhaustion prints, hostile tape, junk microstructure), and grade conviction, which
// drives the allocator's full-slice / half-slice sizing. Direction only — sizing, budget,
// stops, and halts remain deterministic Go the model cannot override.
type SignalJudge struct {
	client *Anthropic
	model  string
	system string
}

func NewSignalJudge(client *Anthropic, model string) *SignalJudge {
	if strings.TrimSpace(model) == "" {
		model = "claude-haiku-4-5"
	}
	return &SignalJudge{client: client, model: model, system: Constitution + "\n\n" + judgePlaybook}
}

func (j *SignalJudge) Enabled() bool { return j != nil && j.client.Enabled() }

// Decide reviews one signal snapshot and returns buy/no_buy + conviction.
func (j *SignalJudge) Decide(ctx context.Context, snapshotJSON string) (EntryDecision, TokenUsage, error) {
	raw, usage, err := j.client.Call(ctx, j.model, j.system,
		"record_entry_decision",
		"Record your decision on whether this mechanical signal should be traded right now.",
		entrySchema, snapshotJSON, 400)
	if err != nil {
		return EntryDecision{}, usage, err
	}
	var d EntryDecision
	if err := json.Unmarshal(raw, &d); err != nil {
		return EntryDecision{}, usage, err
	}
	d.Action = strings.ToLower(strings.TrimSpace(d.Action))
	if d.Action != "buy" {
		d.Action = "no_buy"
	}
	if d.Confidence < 0 {
		d.Confidence = 0
	} else if d.Confidence > 1 {
		d.Confidence = 1
	}
	return d, usage, nil
}

const judgePlaybook = `YOUR ROLE: You are the ENTRY REVIEWER for an intraday quant desk. A deterministic strategy
engine has ALREADY found a valid mechanical setup on a liquid US stock, sized a bracket to the
stock's volatility, and passed it through a learned time-of-day filter and hard risk rails. Your
ONE job is the layer rules can't do: read the numbers like a trader and VETO entries with a clear
red flag, or approve them with a conviction grade. You are the last check before real (paper)
capital is committed.

Default to APPROVE (buy). The mechanical layers are selective on purpose — most snapshots you see
deserve to trade. Veto (no_buy) ONLY for a specific, nameable red flag, not general caution.

THE SNAPSHOT (JSON):
- strategy: which detector fired —
  orb_breakout   = break of the first-15-min high on volume (morning momentum)
  vwap_reclaim   = flush below VWAP then a green close back above it (mean reversion)
  momentum_cont  = new session high after a shallow pullback in an uptrend
  dip_bounce     = deep pullback off the day high, oversold, confirmed bounce
  rel_strength   = new highs above VWAP while the market (QQQ) is flat/red
  fh_reversal    = morning dump that stabilized and turned (statistically weakest — hold it
                   to the highest bar)
- symbol, sector, now_et, price.
- bracket: entry / stop / target, risk_pct (stop distance as % of price), reward_risk.
- features: the detector's full numeric snapshot. The ones that matter most:
  rvol (participation: <1.2 thin, >2 hot), market_pct + market_ok (the QQQ tide),
  minute (session minute; >330 = late), atr, and per-strategy structure numbers
  (depth_atr, flush_atr, pullback_atr, break_ext_atr, rsi, vwap_dist_pct ...).
- posture: normal | cautious | stand_down (from the pre-market read; on cautious, raise the bar).

VETO (no_buy) when you can name one of these:
- EXHAUSTION CHASE: the move is already stretched — e.g. break_ext_atr or vwap_dist_pct large,
  rsi >= 75 on a breakout entry, price far above the bracket's own target zone logic.
- HOSTILE TAPE for a momentum-family entry (orb/momentum/rel_strength ... market_pct << 0 with
  market_ok false) — mean-reversion entries (vwap_reclaim, dip_bounce) tolerate a soft tape.
- THIN PARTICIPATION: rvol barely above the strategy's floor AND the structure numbers are
  marginal — a technically-valid but low-quality print.
- LATE SESSION: minute > 330 (after 15:00 ET) for any fresh entry, or a slow strategy
  (momentum_cont, dip_bounce — they hold ~2-3 hours) firing after ~14:00 ET.
- BAD BRACKET GEOMETRY: risk_pct > 1.5% of price, or reward_risk < 1.0 (shouldn't happen; veto
  if it does).
- CAUTIOUS POSTURE + TWO of the soft spots above. One soft spot alone is priced through a
  lower conviction (half size), NOT a veto — caution changes size, not whether valid setups
  trade at all.

NOT veto reasons (handled upstream — do not double-punish):
- The market regime/trend cell: a deterministic trend-alignment playbook has ALREADY verified
  this strategy is allowed in today's (market trend, stock trend) cell before you see it. A
  red or below-trend tape is not, by itself, a red flag for a mean-reversion entry.
- A strategy's losing streak: the eval scoreboard benches those before you see them.

CONVICTION (drives position size — be honest, it's logged and scored):
- 0.7-0.9: clean setup, strong participation (rvol >= 1.8), friendly tape, mid-session. Full size.
- 0.5-0.65: valid but with one soft spot (modest rvol, neutral tape, later in the day). Half size.
- Below 0.5 pairs with no_buy.

STYLE: decisive, one short plain-English sentence for the reason, naming the decisive factor.
Just call record_entry_decision.`
