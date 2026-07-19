"""Breadcrumbs desk — shared feature & label math (single source of truth).

Both the trainer (train_breadcrumbs_model.py) and the live scorer
(breadcrumbs_live_signals.py) import from here so the features the model is TRAINED on are
byte-for-byte the features it is SCORED on. This parity is the whole point of the module —
never fork the math into two files.

Formulas are identical to the validated walk-forward study (scratchpad wf_engine.build)
and the original SNDK pipeline (ml/sndk_live_signals.py). Percentage triple-barrier labels
(SNDK-equivalent: +0.57% before -0.71% within 5 minutes, same trading day).
"""
import numpy as np
import pandas as pd

# The 9 scale-free features, in the exact order the model expects.
FEAT = ['Z_Score', 'RSI_5', 'RSI_14', 'ROC_3', 'ROC_10', 'ATR_Ratio',
        'MACD_Hist', 'Z_BB', 'Vol_Ratio']

# Percentage triple-barrier constants (validated winner). Keep in sync with the Go exit
# dials (BC_TP_PCT / BC_SL_PCT) — the label the model learns must match how the desk exits.
TP = 0.0057      # +0.57% target (arms the trail)
SL = 0.0071      # -0.71% hard stop
HORIZON = 5      # bars (minutes) the barrier looks forward


def rth(df):
    """Keep only regular-hours bars (09:30–16:00 ET). Index must be tz-aware ET."""
    return df[((df.index.hour > 9) | ((df.index.hour == 9) & (df.index.minute >= 30)))
              & (df.index.hour < 16)]


def compute_features(df):
    """Add all 9 features + EMA_100 + VWAP/VWAP_Std to an OHLCV frame (ET DatetimeIndex).

    Does NOT drop NaNs or filter RTH — the caller decides (training RTH-filters first; the
    live scorer feeds already-RTH streamed bars). Backward-looking only: no lookahead.
    """
    df = df.copy()
    # Intraday VWAP (resets each calendar day) + a rolling dispersion for the VWAP gate.
    df['PV'] = df['Close'] * df['Volume']
    df['Cum_Vol'] = df.groupby(df.index.date)['Volume'].cumsum()
    df['Cum_PV'] = df.groupby(df.index.date)['PV'].cumsum()
    df['VWAP'] = df['Cum_PV'] / (df['Cum_Vol'] + 1e-9)
    df['VWAP_Std'] = df['Close'].rolling(20).std().bfill()

    # Z_Score: Close vs EMA10 in short-window std units.
    df['EMA_10'] = df['Close'].ewm(span=10, adjust=False).mean()
    df['EMA_Std'] = df['Close'].rolling(10).std().bfill()
    df['Z_Score'] = (df['Close'] - df['EMA_10']) / (df['EMA_Std'] + 1e-9)

    # ATR ratio (fast vs slow) — is volatility expanding right now.
    hl = df['High'] - df['Low']
    hc = (df['High'] - df['Close'].shift(1)).abs()
    lc = (df['Low'] - df['Close'].shift(1)).abs()
    tr = pd.concat([hl, hc, lc], axis=1).max(axis=1)
    df['ATR_5'] = tr.rolling(5).mean().bfill()
    df['ATR_20'] = tr.rolling(20).mean().bfill()
    df['ATR_Ratio'] = df['ATR_5'] / (df['ATR_20'] + 1e-9)

    # RSI (fast + slow), Wilder-style rolling-mean form (matches the validated pipeline).
    d = df['Close'].diff()
    for w, nm in ((5, 'RSI_5'), (14, 'RSI_14')):
        g = (d.where(d > 0, 0)).rolling(w).mean()
        l = (-d.where(d < 0, 0)).rolling(w).mean()
        df[nm] = (100 - 100 / (1 + g / (l + 1e-9))).bfill()

    # Rate of change (velocity) over 3 and 10 bars.
    df['ROC_3'] = (df['Close'] - df['Close'].shift(3)) / (df['Close'].shift(3) + 1e-9) * 100
    df['ROC_10'] = (df['Close'] - df['Close'].shift(10)) / (df['Close'].shift(10) + 1e-9) * 100

    # MACD histogram (momentum acceleration).
    df['EMA_12'] = df['Close'].ewm(span=12, adjust=False).mean()
    df['EMA_26'] = df['Close'].ewm(span=26, adjust=False).mean()
    df['MACD'] = df['EMA_12'] - df['EMA_26']
    df['MACD_Signal'] = df['MACD'].ewm(span=9, adjust=False).mean()
    df['MACD_Hist'] = df['MACD'] - df['MACD_Signal']

    # Bollinger position (second stretch read) + volume participation.
    df['BB_Mean'] = df['Close'].rolling(20).mean()
    df['BB_Std'] = df['Close'].rolling(20).std()
    df['Z_BB'] = (df['Close'] - df['BB_Mean']) / (df['BB_Std'] + 1e-9)
    df['Vol_MA5'] = df['Volume'].rolling(5).mean().bfill()
    df['Vol_MA20'] = df['Volume'].rolling(20).mean().bfill()
    df['Vol_Ratio'] = df['Vol_MA5'] / (df['Vol_MA20'] + 1e-9)

    # Trend filter reference (entry gate: Close > EMA_100).
    df['EMA_100'] = df['Close'].ewm(span=100, adjust=False).mean()
    return df


def label_percent(df, barrier=TP, horizon=HORIZON):
    """SYMMETRIC percentage triple-barrier label: 1 if +barrier is hit before -barrier within
    `horizon` bars (same trading day), else 0. Forward-looking BUT capped to the same day so a
    label can never leak across a train/test boundary.

    The barrier is SYMMETRIC and equals the TARGET (±0.57%) — faithful to BOTH the original
    SNDK trainer (symmetric ±$8) and the validated walk-forward study (symmetric ±TP). The
    asymmetric hard stop (−SL = −0.71%) belongs to the EXIT only, NOT the label; the label
    asks "was this a good long setup" symmetrically, and the exit then rides winners with the
    trail and stops losers at −SL.
    """
    hi = df['High'].values
    lo = df['Low'].values
    cl = df['Close'].values
    dts = df.index
    lab = np.zeros(len(df), dtype=int)
    for i in range(len(df) - horizon):
        e = cl[i]
        up = e * (1 + barrier)
        dn = e * (1 - barrier)
        for k in range(1, horizon + 1):
            if dts[i + k].date() != dts[i].date():
                break
            if lo[i + k] <= dn:
                lab[i] = 0
                break
            if hi[i + k] >= up:
                lab[i] = 1
                break
    return lab


def frame_from_bars(bars):
    """Build an ET-indexed OHLCV frame from a list of candle dicts
    ({time(unix s),open,high,low,close,volume}) OR the {O,H,L,C,V} tuple/list form used by
    the fetch pickles. Accepts both so trainer and scorer share one loader.
    """
    if bars and isinstance(bars[0], dict):
        df = pd.DataFrame(bars).rename(columns={
            'open': 'Open', 'high': 'High', 'low': 'Low', 'close': 'Close', 'volume': 'Volume'})
        t = df['time']
    else:
        df = pd.DataFrame(bars, columns=['time', 'Open', 'High', 'Low', 'Close', 'Volume'])
        t = df['time']
    df['datetime'] = pd.to_datetime(t, unit='s', utc=True).dt.tz_convert('America/New_York')
    df.set_index('datetime', inplace=True)
    return df[['Open', 'High', 'Low', 'Close', 'Volume']]
