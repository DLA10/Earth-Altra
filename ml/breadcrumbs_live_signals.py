"""Breadcrumbs desk — batch live signal scorer (multi-symbol, one process per scan).

Reads a batch file {symbol: [candle dicts]}, computes features via the SHARED module (parity
with training), scores each symbol's LAST bar with breadcrumbs_model.bin, applies the three
entry gates, and writes {symbol: {...}} to --out. Batching the whole universe into one call
keeps the desk fast as the basket scales (vs one Python process per symbol per minute).

Entry gates (ALL must hold), identical to the validated pipeline:
  prob >= 0.65  AND  Close > EMA_100  AND  |Close - VWAP| / VWAP_std <= 2.0
"""
import os, sys, json, argparse
import numpy as np
import lightgbm as lgb

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from breadcrumbs_features import FEAT, rth, compute_features, frame_from_bars

PROB_MIN = 0.65
VWAP_MAX_SIGMA = 2.0


def score_symbol(bars, bst):
    """Return a signal dict for one symbol's recent bars, or None if not enough history."""
    if not bars or len(bars) < 100:
        return None
    # RTH-filter BEFORE features so live matches training (which RTH-filters). Without this,
    # pre/after-market bars contaminate VWAP (grouped by day) and the rolling windows near the
    # open — a train/serve skew. rth() is a no-op if the engine already fed only RTH bars.
    df = compute_features(rth(frame_from_bars(bars))).dropna()
    if len(df) < 100:  # need ~100 RTH bars for EMA-100 to converge (matches training warmup)
        return None
    last = df.iloc[-1]
    prob = float(bst.predict(last[FEAT].values.reshape(1, -1))[0])
    close = float(last["Close"]); ema100 = float(last["EMA_100"])
    vwap = float(last["VWAP"]); vstd = float(last["VWAP_Std"])
    dist = abs(close - vwap) / (vstd + 1e-9)

    buy = prob >= PROB_MIN and close > ema100 and dist <= VWAP_MAX_SIGMA
    return {"signal": bool(buy), "probability": prob, "close": close,
            "ema_100": ema100, "vwap": vwap, "vwap_dist": float(dist)}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--batch", required=True, help="JSON file: {symbol: [candle dicts]}")
    ap.add_argument("--out", required=True, help="output JSON: {symbol: signal}")
    ap.add_argument("--model", default=os.path.join(HERE, "breadcrumbs_model.bin"))
    args = ap.parse_args()

    if not os.path.exists(args.model):
        print(f"ERROR: model not found: {args.model}"); sys.exit(1)
    bst = lgb.Booster(model_file=args.model)

    with open(args.batch) as f:
        batch = json.load(f)

    out = {}
    for sym, bars in batch.items():
        try:
            r = score_symbol(bars, bst)
            if r is not None:
                out[sym] = r
        except Exception as e:  # one bad symbol must never kill the whole scan
            print(f"  score error {sym}: {str(e)[:60]}")
    with open(args.out, "w") as f:
        json.dump(out, f)
    n_sig = sum(1 for v in out.values() if v["signal"])
    print(f"scored {len(out)} symbols, {n_sig} buy signals")


if __name__ == "__main__":
    main()
