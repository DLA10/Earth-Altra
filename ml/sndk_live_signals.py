import os
import sys
import json
import argparse
import pandas as pd
import numpy as np
import lightgbm as lgb

def main():
    parser = argparse.ArgumentParser(description="Live signal scoring for SNDK")
    parser.add_argument("--outdir", type=str, required=True, help="Directory to write output signal.json")
    parser.add_argument("--recent-bars", type=str, required=True, help="Path to recent_bars.json containing 1m bars")
    args = parser.parse_args()
    
    if not os.path.exists(args.recent_bars):
        print(f"Error: bars file not found: {args.recent_bars}")
        sys.exit(1)
        
    with open(args.recent_bars, 'r') as f:
        data = json.load(f)
        
    # data is a list of candle dicts: [{"time": unix, "open": X, "high": Y, "low": Z, "close": C, "volume": V}]
    if not data or len(data) < 100:
        print(f"Error: insufficient bar history (need at least 100 bars, got {len(data)})")
        sys.exit(1)
        
    # Standardize column casing
    df = pd.DataFrame(data)
    df = df.rename(columns={
        "open": "Open", "high": "High", "low": "Low", "close": "Close", "volume": "Volume"
    })
    
    # Set up timezone-aware index for Intraday VWAP calculation
    df['datetime'] = pd.to_datetime(df['time'], unit='s', utc=True).dt.tz_convert('America/New_York')
    df.set_index('datetime', inplace=True)
    
    # Calculate Intraday VWAP (resets daily)
    df['PV'] = df['Close'] * df['Volume']
    df['Cum_Vol'] = df.groupby(df.index.date)['Volume'].cumsum()
    df['Cum_PV'] = df.groupby(df.index.date)['PV'].cumsum()
    df['VWAP'] = df['Cum_PV'] / (df['Cum_Vol'] + 1e-9)
    df['VWAP_Std'] = df['Close'].rolling(20).std().bfill()
    
    
    # Calculate indicators
    df['EMA_10'] = df['Close'].ewm(span=10, adjust=False).mean()
    df['EMA_Std'] = df['Close'].rolling(10).std().bfill()
    df['Z_Score'] = (df['Close'] - df['EMA_10']) / (df['EMA_Std'] + 1e-9)
    
    high_low = df['High'] - df['Low']
    high_cp = (df['High'] - df['Close'].shift(1)).abs()
    low_cp = (df['Low'] - df['Close'].shift(1)).abs()
    tr = pd.concat([high_low, high_cp, low_cp], axis=1).max(axis=1)
    df['ATR_5'] = tr.rolling(5).mean().bfill()
    df['ATR_20'] = tr.rolling(20).mean().bfill()
    df['ATR_Ratio'] = df['ATR_5'] / (df['ATR_20'] + 1e-9)
    
    delta = df['Close'].diff()
    gain_5 = (delta.where(delta > 0, 0)).rolling(window=5).mean()
    loss_5 = (-delta.where(delta < 0, 0)).rolling(window=5).mean()
    rs_5 = gain_5 / (loss_5 + 1e-9)
    df['RSI_5'] = 100 - (100 / (1 + rs_5))
    df['RSI_5'] = df['RSI_5'].bfill()
    
    gain_14 = (delta.where(delta > 0, 0)).rolling(window=14).mean()
    loss_14 = (-delta.where(delta < 0, 0)).rolling(window=14).mean()
    rs_14 = gain_14 / (loss_14 + 1e-9)
    df['RSI_14'] = 100 - (100 / (1 + rs_14))
    df['RSI_14'] = df['RSI_14'].bfill()
    
    df['ROC_3'] = (df['Close'] - df['Close'].shift(3)) / (df['Close'].shift(3) + 1e-9) * 100
    df['ROC_10'] = (df['Close'] - df['Close'].shift(10)) / (df['Close'].shift(10) + 1e-9) * 100
    
    df['EMA_12'] = df['Close'].ewm(span=12, adjust=False).mean()
    df['EMA_26'] = df['Close'].ewm(span=26, adjust=False).mean()
    df['MACD'] = df['EMA_12'] - df['EMA_26']
    df['MACD_Signal'] = df['MACD'].ewm(span=9, adjust=False).mean()
    df['MACD_Hist'] = df['MACD'] - df['MACD_Signal']
    
    df['BB_Mean'] = df['Close'].rolling(20).mean()
    df['BB_Std'] = df['Close'].rolling(20).std()
    df['Z_BB'] = (df['Close'] - df['BB_Mean']) / (df['BB_Std'] + 1e-9)
    
    df['Vol_MA5'] = df['Volume'].rolling(5).mean().bfill()
    df['Vol_MA20'] = df['Volume'].rolling(20).mean().bfill()
    df['Vol_Ratio'] = df['Vol_MA5'] / (df['Vol_MA20'] + 1e-9)
    
    df['EMA_100'] = df['Close'].ewm(span=100, adjust=False).mean()
    
    df = df.dropna()
    if df.empty:
        print("Error: empty dataframe after calculating features")
        sys.exit(1)
        
    last_row = df.iloc[-1]
    
    feature_cols = [
        'Z_Score', 'RSI_5', 'RSI_14', 'ROC_3', 'ROC_10', 'ATR_Ratio', 
        'MACD_Hist', 'Z_BB', 'Vol_Ratio'
    ]
    
    # Load model
    model_path = os.path.join(os.path.dirname(__file__), "sndk_lgbm_model.bin")
    if not os.path.exists(model_path):
        print(f"Error: model binary not found at {model_path}")
        sys.exit(1)
        
    bst = lgb.Booster(model_file=model_path)
    
    # Prepare input feature vector
    X = last_row[feature_cols].values.reshape(1, -1)
    
    # Predict probability
    prob = bst.predict(X)[0]
    
    close_price = float(last_row["Close"])
    ema_100_val = float(last_row["EMA_100"])
    vwap_val = float(last_row["VWAP"])
    vwap_std_val = float(last_row["VWAP_Std"])
    
    # Check if probability is >= 0.65 for buy signal
    buy_signal = bool(prob >= 0.65)
    
    # EMA 100 Trend Filter (Only enter Longs if Close > EMA_100)
    if close_price <= ema_100_val:
        if buy_signal:
            print(f"EMA Trend Filter Blocked: Close {close_price:.2f} <= EMA_100 {ema_100_val:.2f}")
        buy_signal = False
        
    # VWAP Distance Filter (The Ultimate Armor)
    dist = abs(close_price - vwap_val) / (vwap_std_val + 1e-9)
    if dist > 2.0:
        if buy_signal:
            print(f"VWAP Distance Filter Blocked: Distance {dist:.2f} > 2.0σ (VWAP: {vwap_val:.2f})")
        buy_signal = False
    
    output_data = {
        "signal": buy_signal,
        "probability": float(prob),
        "close": close_price,
        "ema_100": ema_100_val,
        "vwap": vwap_val,
        "vwap_dist": float(dist)
    }
    
    os.makedirs(args.outdir, exist_ok=True)
    
    out_file = os.path.join(args.outdir, "signal.json")
    with open(out_file, 'w') as f:
        json.dump(output_data, f, indent=2)
        
    print(f"Signal calculated: {buy_signal} (Prob: {prob:.4f}, Close: {close_price}). Saved to {out_file}")

if __name__ == "__main__":
    main()
