import os
import json
import argparse
import pandas as pd
import numpy as np
import joblib

# Throughput mode 2026-07-16: entry stretch 2.5σ → 2.0σ (GS peaked at 2.46σ on 07-16 and
# never fired; 0 signals in all live scans at 2.5). Original value: 2.5 — set RBT_Z_ENTRY=2.5
# to roll back. Must stay in sync with the label threshold in rbt_train.py (same env var).
Z_ENTRY = float(os.getenv("RBT_Z_ENTRY", "2.0"))

# Universe: shared module (legacy 100 ∪ curated liquid baseline ≈ 210 names — 200 plan
# 2026-07-20). Keeps trainer/scorer/Go in lockstep; scorer still narrows to whatever
# columns exist in history_closes.csv below, so a not-yet-retrained cache stays safe.
from rbt_universe import UNIVERSE

def main():
    parser = argparse.ArgumentParser(description="Live Fast RBT Scoring Engine")
    parser.add_argument("--outdir", default="backend/data/rbt", help="Output directory")
    parser.add_argument("--live-prices", default="backend/data/rbt/live_prices.json", help="Path to today's live prices JSON")
    args = parser.parse_args()
    
    models_dir = os.path.join(args.outdir, "models")
    
    # 1. Load trained models and parameters. NOTE: only artifacts rbt_train.py actually
    # writes may be required here — the old vae_scaler.pkl load made the scorer die on
    # any fresh training run (the nightly trainer never produces that file).
    try:
        clf = joblib.load(os.path.join(models_dir, "lgbm_model.pkl"))
        with open(os.path.join(models_dir, "clusters.json"), "r") as f:
            clusters_cfg = json.load(f)
            ticker_clusters = clusters_cfg["ticker_clusters"]
            cluster_groups = {int(k): v for k, v in clusters_cfg["cluster_groups"].items()}
        with open(os.path.join(models_dir, "garch_params.json"), "r") as f:
            garch_cfg = json.load(f)
    except FileNotFoundError as e:
        print(f"Pre-trained RBT models not found: {e}. Please run ml/rbt_train.py first.")
        return
        
    # 2. Read live prices dictionary written by Go backend
    if not os.path.exists(args.live_prices):
        print(f"Live prices JSON file not found: {args.live_prices}.")
        return
    with open(args.live_prices, "r") as f:
        live_prices = json.load(f)
        
    history_closes = pd.read_csv(os.path.join(args.outdir, "history_closes.csv"), index_col=0, parse_dates=True)
    history_highs = pd.read_csv(os.path.join(args.outdir, "history_highs.csv"), index_col=0, parse_dates=True)
    history_lows = pd.read_csv(os.path.join(args.outdir, "history_lows.csv"), index_col=0, parse_dates=True)
    history_vols = pd.read_csv(os.path.join(args.outdir, "history_vols.csv"), index_col=0, parse_dates=True)
    
    global UNIVERSE
    UNIVERSE = [t for t in UNIVERSE if t in history_closes.columns]
    
    # Create today's date index
    today_date = pd.Timestamp.now(tz="America/New_York").tz_localize(None).normalize()
    
    # Create the single row representing today's live closing prices
    today_closes = {}
    today_highs = {}
    today_lows = {}
    today_vols = {}
    
    for ticker in UNIVERSE:
        # Fallback to last close if live price is missing
        last_val = history_closes[ticker].iloc[-1]
        today_closes[ticker] = float(live_prices.get(ticker, {}).get("close", last_val))
        today_highs[ticker] = float(live_prices.get(ticker, {}).get("high", live_prices.get(ticker, {}).get("close", last_val)))
        today_lows[ticker] = float(live_prices.get(ticker, {}).get("low", live_prices.get(ticker, {}).get("close", last_val)))
        today_vols[ticker] = float(live_prices.get(ticker, {}).get("volume", 0))
        
    # Append today's data to historical frames
    df_closes = pd.concat([history_closes, pd.DataFrame([today_closes], index=[today_date])])
    df_highs = pd.concat([history_highs, pd.DataFrame([today_highs], index=[today_date])])
    df_lows = pd.concat([history_lows, pd.DataFrame([today_lows], index=[today_date])])
    df_vols = pd.concat([history_vols, pd.DataFrame([today_vols], index=[today_date])])
    
    # Compute features for each stock (only need the last day's features!)
    signals = []
    feature_cols = ['Z_5', 'Z_GARCH', 'ATR_Ratio', 'BB_Width', 'Rel_Vol', 'RSI_14']
    
    # Calculate GARCH Volatilities recursively up to today
    garch_vol_today = {}
    for ticker in UNIVERSE:
        cfg = garch_cfg.get(ticker, {"omega": 0.05, "alpha": 0.05, "beta": 0.90, "last_vol": 0.02})
        omega = cfg["omega"]
        alpha = cfg["alpha"]
        beta = cfg["beta"]
        last_vol = cfg["last_vol"]
        
        # Calculate volatility recursively for today's price return
        # return today = log(close_today / close_yesterday) * 100
        ret = 100 * np.log(df_closes[ticker].iloc[-1] / (df_closes[ticker].iloc[-2] + 1e-9))
        sig2 = omega + alpha * (ret**2) + beta * (last_vol**2)
        garch_vol_today[ticker] = np.sqrt(sig2) / 100.0
        
    # Calculate normalized spread indices
    norm_closes = {}
    for ticker in UNIVERSE:
        norm_closes[ticker] = df_closes[ticker] / (df_closes[ticker].iloc[0] + 1e-9)
    df_norm = pd.DataFrame(norm_closes)
    
    cluster_indices = {}
    for cid, members in cluster_groups.items():
        cluster_indices[cid] = df_norm[members].mean(axis=1)
        
    # Process features for all tickers
    for ticker in UNIVERSE:
        cid = ticker_clusters.get(ticker, -1)
        if cid == -1:
            continue
            
        closes = df_closes[ticker]
        highs = df_highs[ticker]
        lows = df_lows[ticker]
        vols = df_vols[ticker]
        
        # Mean 20 and Std 20
        mean_20 = closes.rolling(20).mean().iloc[-1]
        std_20 = closes.rolling(20).std().iloc[-1]
        
        z_20 = (closes.iloc[-1] - mean_20) / (std_20 + 1e-9)
        z_5 = (closes.iloc[-1] - closes.rolling(5).mean().iloc[-1]) / (closes.rolling(5).std().iloc[-1] + 1e-9)
        
        # ATR ratios
        high_low = highs - lows
        high_cp = (highs - closes.shift(1)).abs()
        low_cp = (lows - closes.shift(1)).abs()
        tr = pd.concat([high_low, high_cp, low_cp], axis=1).max(axis=1)
        
        atr_5 = tr.rolling(5).mean().iloc[-1]
        atr_20 = tr.rolling(20).mean().iloc[-1]
        atr_ratio = atr_5 / (atr_20 + 1e-9)
        bb_width = (4 * std_20) / (mean_20 + 1e-9)
        
        # Relative Volume
        rel_vol = vols.iloc[-1] / (vols.rolling(20).mean().iloc[-1] + 1e-9)
        
        # RSI 14
        delta = closes.diff()
        gain = (delta.where(delta > 0, 0)).rolling(window=14).mean().iloc[-1]
        loss = (-delta.where(delta < 0, 0)).rolling(window=14).mean().iloc[-1]
        rs = gain / (loss + 1e-9)
        rsi_14 = 100 - (100 / (1 + rs))
        
        # Spread calculation
        c_index_val = cluster_indices[cid].iloc[-1]
        norm_price = closes.iloc[-1] / (df_closes[ticker].iloc[0] + 1e-9)
        spread_val = norm_price - c_index_val
        
        # Spread Rolling Mean & Std
        spread_series = df_norm[ticker] - cluster_indices[cid]
        rolling_mean_spread = spread_series.rolling(20).mean().iloc[-1]
        rolling_std_spread = spread_series.rolling(20).std().iloc[-1]
        
        z_spread = (spread_val - rolling_mean_spread) / (rolling_std_spread + 1e-9)
        
        # Features mapping
        # Z_GARCH represents our spread Z-score
        feats = np.array([z_5, z_spread, atr_ratio, bb_width, rel_vol, rsi_14]).reshape(1, -1)
        
        # Predict probability
        prob = clf.predict_proba(feats)[0, 1]
        
        # Signals trigger check (Z-spread deviations; threshold via RBT_Z_ENTRY, default 2.0)
        if z_spread < -Z_ENTRY:
            direction = "Long"
        elif z_spread > Z_ENTRY:
            direction = "Short"
        else:
            continue
            
        # TP and Stop Loss
        # Stop loss strategy exit: 1.5x ATR
        atr = tr.rolling(20).mean().iloc[-1]
        
        # Target price: price when spread reverts back to its rolling mean spread
        # spread = norm_price - c_index = rolling_mean_spread
        # norm_price = c_index + rolling_mean_spread
        # price = (c_index + rolling_mean_spread) * initial_price
        target_norm = c_index_val + rolling_mean_spread
        target_price = target_norm * df_closes[ticker].iloc[0]
        
        if direction == "Long":
            stop_loss = float(closes.iloc[-1] - 1.5 * atr)
        else:
            stop_loss = float(closes.iloc[-1] + 1.5 * atr)
            
        signals.append({
            "ticker": ticker,
            "direction": direction,
            "probability": float(prob),
            "close": float(closes.iloc[-1]),
            "z_val": float(z_spread),
            "target": float(target_price),
            "stop_loss": float(stop_loss)
        })
        
    out_path = os.path.join(args.outdir, "signals_today.json")
    with open(out_path, "w") as f:
        json.dump(signals, f, indent=4)
        
    print(f"Wrote {len(signals)} fast signals to {out_path}")

if __name__ == "__main__":
    main()
