# Trading Strategy Engine: Bollinger & RSI Hybrid

This document contains the logic for the "Combo" strategy used in our simulations. You can integrate this into your Alpaca Live Execution engine.

## 1. Python Execution Logic (Alpaca Integration)
Use this logic inside your bot to calculate the triggers every minute.

```python
import pandas as pd
import numpy as np

def calculate_indicators(df):
    # 1. BOLLINGER BANDS (20-period, 2 Std Dev)
    df['SMA20'] = df['Close'].rolling(window=20).mean()
    df['STD'] = df['Close'].rolling(window=20).std()
    df['BB_Upper'] = df['SMA20'] + (df['STD'] * 2)
    df['BB_Lower'] = df['SMA20'] - (df['STD'] * 2)

    # 2. RSI (14-period)
    delta = df['Close'].diff()
    gain = (delta.where(delta > 0, 0)).rolling(window=14).mean()
    loss = (-delta.where(delta < 0, 0)).rolling(window=14).mean()
    rs = gain / loss
    df['RSI'] = 100 - (100 / (1 + rs))
    
    return df

def get_signal(row):
    # BUY SIGNAL: Price hits bottom guardrail OR sellers are exhausted
    if (row['Close'] <= row['BB_Lower']) or (row['RSI'] <= 30):
        return "BUY"
    
    # SELL SIGNAL: Price hits high guardrail OR buyers are exhausted
    if (row['Close'] >= row['BB_Upper']) or (row['RSI'] >= 70):
        return "SELL"
    
    return "WAIT"
```

## 2. Visual Charting (TradingView Pine Script)
To see the lines on your chart exactly like the bot sees them, copy-paste this code into the **Pine Editor** at the bottom of TradingView.

```pinescript
//@version=5
indicator("Pilot Console: Bollinger + RSI Combo", overlay=true)

// 1. Bollinger Bands Calculation
length = 20
mult = 2.0
src = close
basis = ta.sma(src, length)
dev = mult * ta.stdev(src, length)
upper = basis + dev
lower = basis - dev

// Plot Bollinger Bands on Chart
plot(basis, "Middle Road", color=color.gray, linestyle=plot.style_linebr)
p1 = plot(upper, "High Guardrail", color=color.red, linewidth=2)
p2 = plot(lower, "Safety Guardrail", color=color.green, linewidth=2)
fill(p1, p2, color=color.rgb(33, 150, 243, 90), title="Highway")

// 2. RSI Calculation (Displayed on a separate pane)
rsi_val = ta.rsi(src, 14)

// 3. Visual Alerts (Arrows on the Candlesticks)
buy_signal = close <= lower or rsi_val <= 30
sell_signal = close >= upper or rsi_val >= 70

plotshape(buy_signal, title="BUY ALERT", style=shape.triangleup, location=location.belowbar, color=color.green, size=size.small, text="ROCK BOTTOM")
plotshape(sell_signal, title="SELL ALERT", style=shape.triangledown, location=location.abovebar, color=color.red, size=size.small, text="THE HIGH")

// 4. RSI Dashboard (Bottom Corner)
var table rsiTable = table.new(position.top_right, 1, 1)
table.cell(rsiTable, 0, 0, "RSI: " + str.tostring(rsi_val, "#.#"), bgcolor=rsi_val < 30 ? color.green : rsi_val > 70 ? color.red : color.gray, text_color=color.white)
```

## 3. How to use the Visual Chart
1. Open **TradingView**.
2. Click **Pine Editor** (bottom bar).
3. Paste the code above and click **Add to Chart**.
4. You will see:
   - **Green/Red lines** for the Bollinger Guardrails.
   - **Green Arrows** whenever the bot would suggest a BUY.
   - **Red Arrows** whenever the bot would suggest a SELL.
   - **Live RSI** number in the top right corner.
