"""Breadcrumbs desk — pooled model trainer (generalized volatility scalper).

Trains ONE LightGBM on the whole volatile basket, pooled, on a trailing ~1-month window
(rolling-1mo, the holdout-validated recipe: pooled >= per-stock; rolling-1mo ~ expanding, so
use the simplest/most-adaptive). Percentage triple-barrier labels. Saves
ml/breadcrumbs_model.bin + ml/breadcrumbs_meta.json. The live scorer hot-loads the .bin.

Usage:
  python train_breadcrumbs_model.py                 # fetch trailing 35d SIP, train, save
  python train_breadcrumbs_model.py --days 35
  python train_breadcrumbs_model.py --pkl a.pkl b.pkl   # train from cached {sym:[bars]} pkls
Data: Alpaca SIP 1-min (keys from backend/.env). RTH only. No lookahead (features backward,
labels capped same-day).
"""
import os, sys, json, time, argparse, pickle
import numpy as np
import pandas as pd
import lightgbm as lgb

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from breadcrumbs_features import (FEAT, TP, SL, HORIZON, rth, compute_features,
                                  label_percent, frame_from_bars)

# The validated 22-name high-volatility basket (kept in sync with BC_UNIVERSE in Go config).
UNIVERSE = ["NVDA", "AMD", "MU", "SMCI", "MRVL", "ARM", "PLTR", "COIN", "TSLA", "MSTR",
            "IONQ", "CRWV", "WDC", "ON", "LRCX", "DELL", "ANET", "SNOW", "HOOD", "RGTI",
            "ASTS", "RIOT"]

MODEL_PATH = os.path.join(HERE, "breadcrumbs_model.bin")
META_PATH = os.path.join(HERE, "breadcrumbs_meta.json")


def fetch(symbols, days):
    """Trailing `days` calendar days of 1-min SIP bars → {sym: ET-OHLCV-frame}."""
    from dotenv import load_dotenv
    from datetime import datetime, timezone, timedelta
    load_dotenv(os.path.join(HERE, "..", "backend", ".env"))
    from alpaca.data.historical import StockHistoricalDataClient
    from alpaca.data.requests import StockBarsRequest
    from alpaca.data.timeframe import TimeFrame, TimeFrameUnit
    from alpaca.data.enums import DataFeed
    key = os.environ["APCA_API_KEY_ID"]; sec = os.environ["APCA_API_SECRET_KEY"]
    cli = StockHistoricalDataClient(key, sec)
    end = datetime.now(timezone.utc)
    start = end - timedelta(days=days)
    out = {}
    for i in range(0, len(symbols), 20):
        ch = symbols[i:i + 20]
        for attempt in range(3):
            try:
                req = StockBarsRequest(symbol_or_symbols=ch,
                                       timeframe=TimeFrame(1, TimeFrameUnit.Minute),
                                       start=start, end=end, feed=DataFeed.SIP)
                r = cli.get_stock_bars(req)
                for s in ch:
                    rows = [(int(b.timestamp.timestamp()), float(b.open), float(b.high),
                             float(b.low), float(b.close), float(b.volume))
                            for b in r.data.get(s, [])]
                    if rows:
                        out[s] = frame_from_bars(rows)
                break
            except Exception as e:
                print(f"  fetch retry {i}: {str(e)[:60]}", flush=True); time.sleep(3)
        print(f"fetched {min(i+20,len(symbols))}/{len(symbols)}", flush=True)
    return out


def from_pkls(paths):
    """Load {sym:[bars]} pickles → {sym: ET-OHLCV-frame} (for offline/initial training)."""
    out = {}
    for p in paths:
        d = pickle.load(open(p, "rb"))
        for s, bars in d.items():
            out[s] = frame_from_bars(bars)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--days", type=int, default=35, help="trailing calendar days to fetch")
    ap.add_argument("--pkl", nargs="*", help="train from cached {sym:[bars]} pkls instead of fetching")
    ap.add_argument("--symbols", type=str, default=",".join(UNIVERSE))
    args = ap.parse_args()
    symbols = [s.strip().upper() for s in args.symbols.split(",") if s.strip()]

    frames = from_pkls(args.pkl) if args.pkl else fetch(symbols, args.days)
    frames = {s: f for s, f in frames.items() if s in symbols}
    if not frames:
        print("ERROR: no data fetched/loaded"); sys.exit(1)

    # Pool: features + % labels per symbol (RTH), keep only the trailing ~1 month if fetched
    # a longer window. Concatenate all symbols into one training set.
    parts = []
    latest = None
    for s, f in frames.items():
        d = compute_features(rth(f))
        d["Label"] = label_percent(d)
        d = d.dropna(subset=FEAT + ["Label"])
        if d.empty:
            continue
        parts.append(d[FEAT + ["Label"]])
        hi = d.index.max()
        latest = hi if latest is None else max(latest, hi)
    if not parts:
        print("ERROR: no usable rows after feature/label build"); sys.exit(1)
    train = pd.concat(parts, ignore_index=True)

    pos = int(train["Label"].sum()); n = len(train)
    print(f"pooled rows: {n} | positive: {pos} ({100*pos/n:.1f}%) | symbols: {len(parts)}", flush=True)

    clf = lgb.LGBMClassifier(n_estimators=50, max_depth=3, learning_rate=0.05,
                             class_weight="balanced", random_state=42, verbosity=-1)
    clf.fit(train[FEAT], train["Label"])
    # Atomic save: write to a temp file then rename, so a live scan can never read a
    # half-written model mid-retrain (it would fail to load and skip that minute).
    tmp = MODEL_PATH + ".tmp"
    clf.booster_.save_model(tmp)
    os.replace(tmp, MODEL_PATH)

    meta = {
        "trained_through": str(latest.date()) if latest is not None else "",
        "symbols": sorted(frames.keys()),
        "rows": n, "positive": pos, "pos_rate": round(pos / n, 4),
        "features": FEAT, "tp_pct": TP, "sl_pct": SL, "horizon_min": HORIZON,
        "model": os.path.basename(MODEL_PATH),
    }
    json.dump(meta, open(META_PATH, "w"), indent=2)
    print(f"saved {MODEL_PATH}\nsaved {META_PATH} (through {meta['trained_through']})", flush=True)


if __name__ == "__main__":
    main()
