import os
import pandas as pd
import numpy as np
import lightgbm as lgb

def main():
    # 1-minute SNDK bars CSV: pass as argv[1] or set SNDK_CSV; no default local path.
    import sys
    csv_path = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("SNDK_CSV", "")
    if not csv_path or not os.path.exists(csv_path):
        print("Error: SNDK 1-minute CSV not found. Usage: train_sndk_production_model.py <path-to-SNDK_1m.csv> (or set SNDK_CSV)")
        return
        
    print(f"Loading data from {csv_path}...")
    df = pd.read_csv(csv_path, index_col=0)
    df.index = pd.to_datetime(df.index)
    df = df.sort_index()
    
    # Calculate features
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
    
    df = df.dropna()
    
    # Labeling
    tp_target = 8.00
    sl_stop = 8.00
    
    labels = []
    closes = df['Close'].values
    highs = df['High'].values
    lows = df['Low'].values
    
    for i in range(len(df)):
        label = 0
        if i < len(df) - 5:
            entry_close = closes[i]
            for offset in range(1, 6):
                future_high = highs[i + offset]
                future_low = lows[i + offset]
                
                if future_low <= entry_close - sl_stop:
                    label = 0
                    break
                if future_high >= entry_close + tp_target:
                    label = 1
                    break
        labels.append(label)
        
    df['Label'] = labels
    
    feature_cols = [
        'Z_Score', 'RSI_5', 'RSI_14', 'ROC_3', 'ROC_10', 'ATR_Ratio', 
        'MACD_Hist', 'Z_BB', 'Vol_Ratio'
    ]
    
    X = df[feature_cols]
    y = df['Label']
    
    print(f"Training LightGBM model on all {len(df)} bars...")
    clf = lgb.LGBMClassifier(n_estimators=50, max_depth=3, learning_rate=0.05, class_weight='balanced', random_state=42, verbosity=-1)
    clf.fit(X, y)
    
    model_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "sndk_lgbm_model.bin")
    clf.booster_.save_model(model_path)
    print(f"Model saved successfully to {model_path}")

if __name__ == "__main__":
    main()
