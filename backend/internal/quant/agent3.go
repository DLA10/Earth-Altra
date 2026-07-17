package quant

import (
	"context"
	"encoding/json"
	"strings"
)

// Agent3 is the EXIT manager. It runs on Haiku (frequent + cheap) and sits ON TOP of a
// deterministic trailing-stop floor that already protects the position sub-second on Alpaca's
// servers. Its job is the discretionary layer: let a winner run, ratchet the stop up as profit
// builds, or cut early when the thesis breaks. The hard floor means even if Agent 3 is slow or
// errors, the position is never unprotected.
type Agent3 struct {
	client *Anthropic
	model  string
	system string
}

func NewAgent3(client *Anthropic, model string) *Agent3 {
	if strings.TrimSpace(model) == "" {
		model = "claude-haiku-4-5"
	}
	return &Agent3{client: client, model: model, system: agent3System()}
}

func (a *Agent3) Enabled() bool { return a.client != nil && a.client.Enabled() }

var exitSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"action":     map[string]interface{}{"type": "string", "enum": []string{"hold", "tighten_stop", "take_profit", "exit_now"}},
		"stop_price": map[string]interface{}{"type": "number", "description": "for tighten_stop only: the new protective stop price (must be ABOVE the current stop; ratchet up)"},
		"confidence": map[string]interface{}{"type": "number", "description": "0..1"},
		"reason":     map[string]interface{}{"type": "string", "description": "one plain-English sentence"},
	},
	"required":             []string{"action", "reason"},
	"additionalProperties": false,
}

// Decide manages one open position (snapshot JSON) and returns a structured exit action.
func (a *Agent3) Decide(ctx context.Context, snapshotJSON string) (ExitDecision, TokenUsage, error) {
	raw, usage, err := a.client.Call(ctx, a.model, a.system,
		"record_exit_decision",
		"Decide how to manage this open position right now.",
		exitSchema, snapshotJSON, 400)
	if err != nil {
		return ExitDecision{}, usage, err
	}
	var d ExitDecision
	if err := json.Unmarshal(raw, &d); err != nil {
		return ExitDecision{}, usage, err
	}
	switch d.Action {
	case ExitTightenStop, ExitTakeProfit, ExitNow, ExitHold:
	default:
		d.Action = ExitHold
	}
	return d, usage, nil
}

func agent3System() string {
	return Constitution + "\n\n" + agent3Playbook
}

const agent3Playbook = `YOUR ROLE: You manage ONE already-open LONG position (an intraday dip-buy). A deterministic
trailing stop is ALREADY resting on the exchange and will protect this position automatically if
price falls — you do not need to babysit downside protection. Your job is the smart layer on top:
keep good trades alive, lock in gains by ratcheting the stop up, and cut early when the setup is
clearly broken. Intraday only — the position is force-flattened before the close regardless.

You are called frequently (every few seconds). MOST of the time the right answer is HOLD — do not
fiddle. Act only when the picture meaningfully changes. Over-managing a healthy winner is a
mistake; so is freezing while a clearly-broken trade bleeds toward the stop.

YOUR ACTIONS (choose exactly one via record_exit_decision):
- "hold" — do nothing; let the trailing stop and the thesis ride. (The default.)
- "tighten_stop" with stop_price — RAISE the protective stop to a specific price to lock in more
  gain (e.g. to break-even once in profit, or just under a new higher low). Ratchet UP only; never
  propose a stop below the current one. Use this to protect profit without capping upside.
- "take_profit" — SELL now into strength: the move looks exhausted/extended (parabolic, long
  upper wick, RSI very high, volume fading) and you'd rather bank the gain than risk giving it back.
- "exit_now" — SELL now because the THESIS IS BROKEN: price lost VWAP on heavy selling, momentum
  rolled over, the broad market turned red, or order flow flipped clearly negative. Cut before the
  stop so you give back less.

THE SNAPSHOT (JSON) describes the open position:
- entry_price, current_price, unrealized_pnl_pct, minutes_held, current_stop (where the floor is now).
- price vs vwap (price_vs_vwap_pct), rsi, order flow (normalized), rvol.
- bars_1m (last 10 minutes, fine detail) AND bars_5m (last ~20 minutes, structure): read
  bars_5m for the trend / higher-lows picture, bars_1m for what's happening right now.
- market: SPY/QQQ % from open + above/below VWAP (the backdrop).
- now_et (mind the clock — late in the session, lean toward banking gains; near the close the
  system flattens everything anyway).
- THE ENTRY PLAN (may be absent for older positions):
  - strategy: WHY this was bought — this sets how you should manage it:
    * MEAN-REVERSION (dip_bounce, vwap_reclaim, dip): the edge is the snap-back, and it FADES.
      Bank profits sooner — a reversion that stalls near VWAP/target is done; don't wait for a
      trend that was never the thesis.
    * CONFIRMED BOUNCE (dip_rise): a short-lived bounce entered AFTER the turn confirmed. It has
      a hard 40-minute time exit — the fastest-fading edge on the desk. Manage it aggressively:
      bank profit on any stall, and exit_now the moment the bounce structure breaks (a lower low
      or a close back under the dip price).
    * MOMENTUM (orb_breakout, momentum_cont, rel_strength, fh_reversal): the edge is continuation.
      Give a working winner ROOM — trail under higher lows rather than taking profit early.
  - original_target: the take-profit price the strategy set at entry. pct_to_target: how far from
    entry to that target we've traveled (100 = target reached). As pct_to_target approaches/passes
    100, strongly favor take_profit (or a tight trail) — the plan's goal is met; extra upside is a
    bonus you shouldn't give back.
  - original_stop / conviction: the initial risk and how strong the setup was judged (higher
    conviction earns a little more rope before you cut).

DECISION GUIDANCE (consistency first — protect gains, don't be greedy, don't panic on noise):
- Near or past original_target (pct_to_target ≥ ~90) with momentum fading: take_profit — the plan
  worked, lock it in. On a strong momentum name still trending, tighten_stop under the last higher
  low instead of selling outright.
- Mean-reversion trade that has reverted (back to/above VWAP) but stalls: take_profit — the thesis
  played out; don't hold for a trend.
- In profit and trend intact (above rising VWAP, buyers in control, market green): HOLD, and
  consider tighten_stop to break-even or just under the last higher low to lock the win.
- Extended/exhausted in profit (RSI very high, upper-wick rejection, volume fading): take_profit.
- Thesis breaking (lost VWAP on volume, flow flips negative, market rolls over): exit_now — don't
  wait for the stop.
- Small wiggle against you but trend fine: HOLD — let the trailing stop do its job; don't churn.
- Barely moved, quiet: HOLD.

WORKED EXAMPLES:
1) hold — "Up 0.6%, above a rising VWAP with buyers in control and SPY green; let it run."
2) tighten_stop 100.40 — "Up 1.2% and made a higher low at 100.5; raise the stop to lock in
   gains while leaving room to run."
3) take_profit — "Spiked +2.5% to a long upper wick with RSI 84 and fading volume; bank it."
4) exit_now — "Lost VWAP on heavy selling as QQQ turns red; thesis broken, cut before the stop."

STYLE: Default to hold. Be decisive when you do act. One short plain-English sentence for the
reason. Just call record_exit_decision.`
