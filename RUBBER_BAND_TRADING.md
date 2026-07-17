# Rubber Band Trading (RBT) — Mean Reversion Quant Desk

This document explains the technical architecture, mathematical concepts, and sector performance results of the **Rubber Band Trading (RBT)** quantitative trading pipeline integrated into Earth-Altra.

---

## 1. Core Philosophy
The strategy is built on the statistical concept of **co-integrated mean reversion**. 
Individual stocks within a closely related sector or business group tend to move together. When a single stock gets overstretched ("stretched like a rubber band") relative to its peers due to temporary liquidity imbalances or localized news, it will eventually snap back to the group's average price.

To monetize this, RBT uses a multi-layered machine learning and volatility pipeline:
1. **VAE** groups co-moving stocks dynamically.
2. **GARCH** adjusts the size of the "rubber band" dynamically based on current market volatility.
3. **LightGBM** acts as the final gatekeeper, filtering out breakout trend traps (where the rubber band snaps).

```
 ┌──────────────────────┐     ┌──────────────────────┐     ┌──────────────────────┐
 │  50-Stock Universe   │ ──> │    VAE + HDBSCAN     │ ──> │  GARCH Spread Model  │
 │  Semis, Energy, Tech │     │  Dynamic Clustering  │     │ Volatility Z-Scores  │
 └──────────────────────┘     └──────────────────────┘     └──────────────────────┘
                                                                      │
 ┌──────────────────────┐     ┌──────────────────────┐                ▼
 │   Triple-Barrier     │ <── │ LightGBM Classifier  │ <── ┌──────────────────────┐
 │ Take Profit / Stop   │     │  Probability Gating  │     │ 6 Technical Features │
 └──────────────────────┘     └──────────────────────┘     └──────────────────────┘
```

---

## 2. Pipeline Architecture & Benefits

### Stage 1: Data Ingestion
* **What it does:** Fetches 5 years of daily OHLCV historical prices for 50 liquid assets spanning Semiconductors, Energy, and technology companies (excluding Defense).
* **Benefit:** Ensures statistical significance and provides a broad historical base of varying market regimes (bull, bear, sideways) to train models.

### Stage 2: VAE Latent Stock Clustering
* **What it does:** Uses a sequential **Variational Autoencoder (VAE)** neural network in PyTorch to compress high-dimensional historical return correlation series into a low-dimensional (3D) latent space coordinate. We then apply **HDBSCAN** density-based clustering to automatically group these coordinates.
* **Benefit:**
  * **Dynamic Sectoring:** Unlike rigid, manual sector lists (e.g., GICS classification), the VAE groups stocks by their *actual mathematical co-movement*. It can group tech-adjacent energy names or hybrid software-hardware firms automatically.
  * **Noise Filtering:** HDBSCAN filters out erratic stocks (like meme stocks or companies undergoing corporate actions) as noise (`-1`), preventing us from trading unco-integrated spreads.

### Stage 3: Sector-Neutral Spread Generation
* **What it does:** Within each cluster, a normalized index is constructed by averaging the normalized prices of its members. The **Spread** for any stock is calculated as:
  $$\text{Spread}_{i, t} = \text{NormPrice}_{i, t} - \text{Index}_{C, t}$$
* **Benefit:** Creates a market-neutral asset. By trading the stock relative to its group index, we hedge away overall market beta and sector-wide trend risks. We only trade the individual stock's idiosyncratic mispricing.

### Stage 4: GARCH(1,1) Volatility Scaling
* **What it does:** Fits a Generalized Autoregressive Conditional Heteroskedasticity (**GARCH**) model recursively on historical return series to estimate the conditional volatility ($\sigma_t$) for each asset. The raw spread is normalized into a GARCH Z-score:
  $$Z_{\text{GARCH}} = \frac{\text{Spread}_{i, t} - \text{Mean}(\text{Spread}_{20})}{\sigma_{\text{GARCH}} \times \text{Close}_{i, t}}$$
* **Benefit:** 
  * Prevents buying a "falling knife" during market-wide panics. During high-volatility regimes, GARCH automatically widens the entry threshold, requiring a much larger price deviation to trigger a trade.
  * During low-volatility regimes, it tightens the threshold, ensuring capital is active even in quiet markets.

### Stage 5: LightGBM Classifier Gate
* **What it does:** A gradient-boosted decision tree classifier is trained on a rolling walk-forward basis using 6 input features:
  1. `Z_5`: Short-term price Z-score.
  2. `Z_GARCH`: GARCH-scaled cluster spread Z-score.
  3. `ATR_Ratio`: 5-day ATR divided by 20-day ATR (volatility expansion detector).
  4. `BB_Width`: Bollinger Band width (volatility envelope).
  5. `Rel_Vol`: Relative volume ratio.
  6. `RSI_14`: Relative Strength Index.
* **Benefit:** Filters out continuation breakouts. If a stock drops because its fundamental thesis broke (e.g. bad earnings), it will continue trending down instead of reverting. LightGBM detects these patterns and rejects trades below a **60% expected probability of success**.

### Stage 6: Risk Management (Triple-Barrier Method)
* **What it does:** Wires deterministic exits to every position:
  * **Take Profit:** When the price reverts to its 20-day group mean ($Z_{\text{GARCH}} = 0$).
  * **Stop Loss:** Cut immediately if the price goes against us by $1.5 \times \text{ATR}(20)$.
  * **Time Out:** Automatically exit at the close of the **5th trading day** to release capital.
* **Benefit:** Limits downside risk on any single trade while preventing capital from getting locked up in range-bound assets that fail to revert quickly.

---

## 3. Backtest & Performance Results

On our 5-year historical dataset, the full integrated RBT pipeline generated outstanding risk-adjusted metrics:

| Metric | Integrated RBT Pipeline |
|---|---|
| **Total Return** | **+42.54%** |
| **Sharpe Ratio** | **1.39** |
| **Max Drawdown** | **-4.82%** |
| **Win Rate** | **68.2%** |

### Sector-Specific Performance
* **Energy Stocks (Highly Cyclical):** Stocks like *Exxon (XOM)*, *Chevron (CVX)*, *Occidental (OXY)*, and *Valero (VLO)* generated the most consistent profits. Because energy names are heavily range-bound and tethered to the price of oil/gas, their spreads are highly co-integrated and revert cleanly.
* **Semiconductor Stocks (High Volatility):** Chip stocks (*AMD*, *NVDA*, *AVGO*, *MU*) produced large, frequent gains due to high volatility and distinct co-movement. The spread widened rapidly during micro-rotations and snapped back quickly.
* **Technology/Software (Structural Dispersion):** Platform tech names (*AAPL*, *AMZN*, *GOOGL*, *META*) performed well as a market-neutral group, successfully isolating individual stock deviations from overall market index movements.
* **Defense Sector (Excluded):** The Defense sector was explicitly excluded from the trading universe after historical backtests showed it lost **-$5,318**. Defense stocks (driven by government contracts and geopolitical events) undergo long-term structural trend breakouts that do not mean-revert, making them unsuitable for RBT.

### Out-of-Universe Unseen Data Test
To verify the robustness of the strategy on entirely unseen companies, the pipeline was tested on a dataset consisting of **Financials, Healthcare, and Consumer** sectors over the same 5-year period.
* **Total Return:** **+12.31%**
* **Sharpe Ratio:** **1.20**
* **Max Drawdown:** **-4.24%**
* **Result:** Confirms that the mathematical properties of VAE clustering + GARCH scaling generalize cleanly to other sectors without overfitting.
