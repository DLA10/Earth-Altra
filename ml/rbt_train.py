import os
import json
import argparse
import yfinance as yf
import pandas as pd
import numpy as np
import joblib
from arch import arch_model
from statsmodels.tsa.stattools import coint

# Throughput mode 2026-07-16 (originals in THROUGHPUT_MODE.md):
# - Z_ENTRY: label trades at the SAME stretch the live scorer uses (was hardcoded 1.5 while
#   live traded 2.5 — the model was scored on a regime it never trades). Shares the
#   RBT_Z_ENTRY env var with rbt_live_signals.py so they cannot drift apart again.
# - MAX_CLUSTER: cap cluster size (was unbounded — connected-components chained 53 of 62
#   names into one blob, diluting every spread). Oversized components are re-split with a
#   progressively stricter correlation bar until every family is <= the cap.
Z_ENTRY = float(os.getenv("RBT_Z_ENTRY", "2.0"))
MAX_CLUSTER = int(os.getenv("RBT_MAX_CLUSTER", "12"))

UNIVERSE = [
    # Semiconductors (20)
    "ADI", "AMD", "AMAT", "ASML", "AVGO", "INTC", "KLAC", "LRCX", "MCHP", "MPWR", 
    "MRVL", "MU", "NVDA", "NXPI", "ON", "QCOM", "SMCI", "TSM", "TXN", "ARM",
    # Energy (20)
    "COP", "CVX", "EOG", "MPC", "OXY", "PSX", "SLB", "VLO", "WMB", "XOM",
    "HAL", "BKR", "AR", "DVN", "FANG", "KMI", "OKE", "APA", "LNG", "EQT",
    # Tech / Software (20)
    "AAPL", "ACN", "ADBE", "AMZN", "ANET", "CRM", "CSCO", "GOOGL", "IBM", "INTU", 
    "META", "MSFT", "NFLX", "NOW", "ORCL", "PLTR", "SHOP", "SNOW", "UBER", "DELL",
    # Financials (20)
    "JPM", "BAC", "MS", "GS", "C", "WFC", "BK", "SCHW", "COF", "USB",
    "AXP", "BLK", "MET", "PRU", "PNC", "TFC", "FITB", "KEY", "RF", "HBAN",
    # Materials / Mining / Industrials (20)
    "FCX", "NEM", "NUE", "AA", "ALB", "CLF", "STLD", "MLM", "VMC", "APD",
    "CAT", "DE", "HON", "EMR", "ETN", "GE", "ITW", "PH", "ROK", "PWR"
]

def calculate_garch_volatility(df, train_dates):
    close_prices = df['Close'].reindex(train_dates).ffill().bfill().values
    returns = 100 * np.diff(np.log(close_prices + 1e-9))
    returns = np.nan_to_num(returns, nan=0.0)
    
    # Fit GARCH(1,1)
    am = arch_model(returns, vol='Garch', p=1, q=1, dist='normal')
    res = am.fit(update_freq=0, disp='off')
    return res.params, float(res.conditional_volatility[-1])

def generate_labels(df, lookahead=5, stop_mult=1.5):
    # Dynamic barrier labeling at the LIVE entry stretch (Z_ENTRY), so the classifier's
    # probability is calibrated on exactly the setups it will be asked to score.
    atr = df['ATR_20']
    close = df['Close']
    labels = []

    for i in range(len(df) - lookahead):
        curr_close = close.iloc[i]
        curr_atr = atr.iloc[i]
        curr_z = df['Z_GARCH'].iloc[i]

        target = df['Mean_20'].iloc[i]
        stop = 1.5 * curr_atr

        outcome = 0 # default exit is time/neutral

        # Determine trade setup: Z_GARCH determines direction
        if curr_z < -Z_ENTRY:
            # Long entry
            for t in range(1, lookahead + 1):
                future_close = close.iloc[i + t]
                if future_close >= target:
                    outcome = 1 # target hit (win)
                    break
                if future_close <= curr_close - stop:
                    outcome = 0 # stop loss hit (loss)
                    break
        elif curr_z > Z_ENTRY:
            # Short entry
            for t in range(1, lookahead + 1):
                future_close = close.iloc[i + t]
                if future_close <= target:
                    outcome = 1 # target hit (win)
                    break
                if future_close >= curr_close + stop:
                    outcome = 0 # stop loss hit (loss)
                    break
                    
        labels.append(outcome)
        
    labels_series = pd.Series(labels, index=df.index[:len(labels)], name="Label")
    return labels_series

def main():
    parser = argparse.ArgumentParser(description="Nightly RBT Co-integration Trainer")
    parser.add_argument("--outdir", default="backend/data/rbt", help="Output directory")
    args = parser.parse_args()
    
    models_dir = os.path.join(args.outdir, "models")
    os.makedirs(models_dir, exist_ok=True)
    
    print("Downloading historical daily data (5y period)...")
    stocks_data = {}
    for ticker in UNIVERSE:
        df = yf.download(ticker, period="5y", progress=False)
        if isinstance(df.columns, pd.MultiIndex):
            df.columns = df.columns.get_level_values(0)
        if not df.empty and len(df) > 100:
            stocks_data[ticker] = df
            
    tickers = list(stocks_data.keys())
    if not tickers:
        print("No stock data downloaded.")
        return
        
    # Save raw historical prices to CSV for fast lookup during live scoring
    history_closes = pd.DataFrame({t: df['Close'] for t, df in stocks_data.items()})
    history_closes.to_csv(os.path.join(args.outdir, "history_closes.csv"))
    
    history_highs = pd.DataFrame({t: df['High'] for t, df in stocks_data.items()})
    history_highs.to_csv(os.path.join(args.outdir, "history_highs.csv"))
    
    history_lows = pd.DataFrame({t: df['Low'] for t, df in stocks_data.items()})
    history_lows.to_csv(os.path.join(args.outdir, "history_lows.csv"))
    
    history_vols = pd.DataFrame({t: df['Volume'] for t, df in stocks_data.items()})
    history_vols.to_csv(os.path.join(args.outdir, "history_vols.csv"))

    all_dates = pd.DatetimeIndex(sorted(list(set().union(*[df.index for df in stocks_data.values()]))))
    split_idx = int(len(all_dates) * 0.75)
    train_dates = all_dates[:split_idx]
    
    # 2. Engle-Granger Co-integration Clustering
    print("Running Engle-Granger co-integration clustering...")
    N = len(tickers)
    prices_list = []
    valid_tickers = []
    
    for t in tickers:
        df_train = stocks_data[t].reindex(train_dates)
        close = pd.Series(df_train['Close'].values.flatten()).ffill().bfill().values
        # normalize price
        close = close / (close[0] + 1e-9)
        prices_list.append(close)
        valid_tickers.append(t)
        
    # Pre-filter: only run the cointegration test on pairs whose returns are already plausibly
    # related (correlation > 0.4). Testing all 1225 pairs at p<0.05 throws ~60 false-positive
    # links by chance, and connected-components then chains unrelated stocks into giant blobs
    # (e.g. a pipeline company grouped with semiconductors). The pre-filter keeps families tight.
    rets = np.array([np.diff(np.log(p + 1e-9)) for p in prices_list])
    corr = np.corrcoef(rets)

    adj = np.zeros((N, N))
    for i in range(N):
        for j in range(i + 1, N):
            if not np.isfinite(corr[i, j]) or corr[i, j] < 0.4:
                continue  # skip implausible pairs before the expensive/spurious coint test
            try:
                _, pval, _ = coint(prices_list[i], prices_list[j], trend='c', autolag='AIC')
            except Exception as e:
                # A degenerate pair (flat/constant series, insufficient overlap, collinearity)
                # must NOT crash the whole nightly retrain. Treat it as not co-integrated and
                # continue so the remaining pairs and the model still get built.
                print(f"  coint skipped {valid_tickers[i]}/{valid_tickers[j]}: {e}")
                continue
            if pval < 0.05:
                adj[i, j] = 1
                adj[j, i] = 1
                
    # Connected Components (BFS)
    def components_of(adjm, nodes):
        seen = set()
        comps = []
        node_set = set(nodes)
        for i in nodes:
            if i in seen:
                continue
            if len([j for j in np.where(adjm[i])[0] if j in node_set]) < 1:
                seen.add(i)
                continue
            comp = []
            queue = [i]
            seen.add(i)
            while queue:
                curr = queue.pop(0)
                comp.append(curr)
                for neighbor in np.where(adjm[curr])[0]:
                    if neighbor in node_set and neighbor not in seen:
                        seen.add(neighbor)
                        queue.append(neighbor)
            comps.append(comp)
        return comps

    def split_oversized(comp, thr):
        # A blob bigger than MAX_CLUSTER dilutes every spread in it. Re-split it using a
        # stricter correlation bar (keep only edges corr >= thr among cointegrated pairs)
        # until every family fits; singletons/pairs that fall off simply drop out.
        if len(comp) <= MAX_CLUSTER or thr > 0.95:
            if len(comp) > MAX_CLUSTER:
                # >MAX_CLUSTER names all pairwise-correlated >=0.95: splitting further is
                # arbitrary, so keep the family — but say so instead of silently exceeding the cap.
                print(f"  cluster cap: keeping {len(comp)}-name family intact (all corr >= 0.95)")
            return [comp] if len(comp) >= 3 else []
        sub_adj = np.zeros_like(adj)
        for a in comp:
            for b in comp:
                if a < b and adj[a, b] and np.isfinite(corr[a, b]) and corr[a, b] >= thr:
                    sub_adj[a, b] = 1
                    sub_adj[b, a] = 1
        out = []
        for sub in components_of(sub_adj, comp):
            if len(sub) >= 3:
                out.extend(split_oversized(sub, thr + 0.05))
        return out

    consensus_clusters = []
    for comp in components_of(adj, list(range(N))):
        if len(comp) < 3:
            continue
        for c in split_oversized(comp, 0.5):
            consensus_clusters.append(c)
    sizes = sorted((len(c) for c in consensus_clusters), reverse=True)
    print(f"Clusters after size cap (max {MAX_CLUSTER}): {len(consensus_clusters)} families, sizes {sizes}")
                
    ticker_clusters = {t: -1 for t in valid_tickers}
    cluster_groups = {}
    for cid, comp in enumerate(consensus_clusters):
        members = [valid_tickers[idx] for idx in comp]
        cluster_groups[cid] = members
        for m in members:
            ticker_clusters[m] = cid
            
    # Save clusters configuration
    with open(os.path.join(models_dir, "clusters.json"), "w") as f:
        json.dump({"ticker_clusters": ticker_clusters, "cluster_groups": {str(k): v for k, v in cluster_groups.items()}}, f)
        
    # 3. Fit GARCH models
    print("Fitting GARCH volatility models...")
    garch_params = {}
    processed_dfs = {}
    
    for ticker in tickers:
        df = stocks_data[ticker]
        params, last_vol = calculate_garch_volatility(df, train_dates)
        garch_params[ticker] = {
            "omega": float(params.get('omega', 0.05)),
            "alpha": float(params.get('alpha[1]', 0.05)),
            "beta": float(params.get('beta[1]', 0.90)),
            "last_vol": float(last_vol)
        }
        
        # Calculate volatility histories for model training
        close_prices = df['Close'].reindex(train_dates).ffill().bfill().values
        returns = 100 * np.diff(np.log(close_prices + 1e-9))
        returns = np.nan_to_num(returns, nan=0.0)
        
        # Recursive filter
        sig2 = last_vol**2
        vols = []
        full_close = df['Close'].ffill().bfill().values
        full_returns = 100 * np.diff(np.log(full_close + 1e-9))
        full_returns = np.nan_to_num(full_returns, nan=0.0)
        
        o = garch_params[ticker]["omega"]
        a = garch_params[ticker]["alpha"]
        b = garch_params[ticker]["beta"]
        
        for r in full_returns:
            sig2 = o + a * (r**2) + b * sig2
            vols.append(np.sqrt(sig2) / 100.0)
            
        df_copy = df.copy()
        df_copy['GARCH_Vol'] = pd.Series([vols[0]] + vols, index=df.index)
        df_copy['Mean_20'] = df_copy['Close'].rolling(20).mean()
        df_copy['Std_20'] = df_copy['Close'].rolling(20).std()
        df_copy['Z_20'] = (df_copy['Close'] - df_copy['Mean_20']) / (df_copy['Std_20'] + 1e-9)
        df_copy['Z_5'] = (df_copy['Close'] - df_copy['Close'].rolling(5).mean()) / (df_copy['Close'].rolling(5).std() + 1e-9)
        df_copy['Dollar_Vol_GARCH'] = df_copy['GARCH_Vol'] * df_copy['Close']
        df_copy['Z_GARCH'] = (df_copy['Close'] - df_copy['Mean_20']) / (df_copy['Dollar_Vol_GARCH'] + 1e-9)
        
        high_low = df_copy['High'] - df_copy['Low']
        high_cp = (df_copy['High'] - df_copy['Close'].shift(1)).abs()
        low_cp = (df_copy['Low'] - df_copy['Close'].shift(1)).abs()
        tr = pd.concat([high_low, high_cp, low_cp], axis=1).max(axis=1)
        
        df_copy['ATR_5'] = tr.rolling(5).mean()
        df_copy['ATR_20'] = tr.rolling(20).mean()
        df_copy['ATR_Ratio'] = df_copy['ATR_5'] / (df_copy['ATR_20'] + 1e-9)
        df_copy['BB_Width'] = (4 * df_copy['Std_20']) / (df_copy['Mean_20'] + 1e-9)
        df_copy['Rel_Vol'] = df_copy['Volume'] / (df_copy['Volume'].rolling(20).mean() + 1e-9)
        
        delta = df_copy['Close'].diff()
        gain = (delta.where(delta > 0, 0)).rolling(window=14).mean()
        loss = (-delta.where(delta < 0, 0)).rolling(window=14).mean()
        rs = gain / (loss + 1e-9)
        df_copy['RSI_14'] = 100 - (100 / (1 + rs))
        
        processed_dfs[ticker] = df_copy
        
    with open(os.path.join(models_dir, "garch_params.json"), "w") as f:
        json.dump(garch_params, f)
        
    # Recalculate spreads on normalized prices
    norm_prices = {}
    for ticker, df in processed_dfs.items():
        prices = df['Close'].reindex(all_dates).ffill().bfill()
        prices_norm = prices / (prices.iloc[0] + 1e-9)
        norm_prices[ticker] = prices_norm
    df_norm = pd.DataFrame(norm_prices)
    
    cluster_indices = {}
    for cid, members in cluster_groups.items():
        cluster_indices[cid] = df_norm[members].mean(axis=1)
        
    for ticker, cid in ticker_clusters.items():
        if cid == -1:
            continue
        df = processed_dfs[ticker]
        c_index = cluster_indices[cid].reindex(df.index)
        norm_price = df['Close'] / (stocks_data[ticker]['Close'].iloc[0] + 1e-9)
        spread = norm_price - c_index
        df['Spread'] = spread
        rolling_mean_spread = spread.rolling(20).mean()
        rolling_std_spread = spread.rolling(20).std()
        df['Z_Spread'] = (spread - rolling_mean_spread) / (rolling_std_spread + 1e-9)
        df['Z_GARCH'] = df['Z_Spread']
        
    # Prepare features and train LightGBM
    print("Training LightGBM Classifier...")
    X_list, y_list = [], []
    feature_cols = ['Z_5', 'Z_GARCH', 'ATR_Ratio', 'BB_Width', 'Rel_Vol', 'RSI_14']
    
    for t in valid_tickers:
        df = processed_dfs[t]
        df_train = df.reindex(train_dates).dropna()
        if df_train.empty:
            continue
        labels = generate_labels(df, lookahead=5, stop_mult=1.5)
        df_labeled = df_train.join(labels, how='inner', lsuffix='_left', rsuffix='_right')
        if df_labeled.empty:
            continue
        label_col = 'Label' if 'Label' in df_labeled.columns else df_labeled.columns[-1]
        X_list.append(df_labeled[feature_cols])
        y_list.append(df_labeled[label_col])
        
    X_train = pd.concat(X_list)
    y_train = pd.concat(y_list)
    
    import lightgbm as lgb
    clf = lgb.LGBMClassifier(n_estimators=100, max_depth=4, learning_rate=0.05, class_weight='balanced', random_state=42, verbosity=-1)
    clf.fit(X_train, y_train)
    
    # Save LightGBM
    joblib.dump(clf, os.path.join(models_dir, "lgbm_model.pkl"))
    print("Training complete! Engle-Granger Co-integration Models saved under backend/data/rbt/models/")

if __name__ == "__main__":
    main()
