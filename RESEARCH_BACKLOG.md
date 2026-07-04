# RESEARCH_BACKLOG.md — the idea queue

## Status ledger (2026-07-03) — settled questions; do not re-litigate

| # | Idea | Verdict |
|---|---|---|
| 1 | First-hour reversal | ❌ **KILLED** (−$515/6mo standalone; open30 analysis: corr +0.017 — no reversal effect in this universe). Detector kept shadow-only; TOD gate benches it |
| 2 | Cross-sectional ranking gate | ➖ **BENCHED — 12-mo retest FAILED** (rank gate at `-mltopq 0.70` on 246 days: accepted-R −0.020 < rejected-R +0.009, i.e. anti-selection; fails the promotion bar again; stays off order flow) |
| 3 | Time-of-day conditioning | ❌ **DEMOTED TO SHADOW** (2026-07-04, pre-registered bar). Cumulative buckets fail across a regime change (12-mo: base −$718 → gated −$961); the decay fix (halflife 30, mirroring cusumDecayN) repairs part of that (−$902) but destroys the gate's recent-window value (Jan–Jul: base −$219, cumulative −$31, decay-30 −$692; Apr–Jul: base +$871, cumulative +$958, decay-30 +$430) — at ~30 outcomes/day over ~78 buckets the gate is data-starved at any forgetting rate. Neither variant beat no-gate on both windows → fallback fired: `QUANT_TOD_GATE` defaults false, buckets keep journaling with decay for a future re-review |
| 4 | Regime mixture-of-experts | ➖ tested as 2-state router (`-router`): −38% drawdown, costs upside; available by flag, not in production |
| 5 | Passive execution | ❌ **KILLED** as pure-passive (12% fill, adverse selection, holdout −$239); chase variant (P2.3) also **KILLED** 2026-07-04: worst of the three execution models on 12 mo (market −$718 · passive −$638 · chase −$802 — chasing near-misses is adverse selection). Market entries stay |
| 6 | Vol-targeted / Kelly throttle | ❌ **KILLED** (EWMA half-sizing: no value at 6mo) |
| 7 | Bigger dataset + recency weighting | ◐ recency weighting **KILLED** (holdout spread +0.009 → −0.037); 12-month dataset **DONE** (Phase-1 Task 4: 17,511 rows Jul 2025–Jul 2026 in `ml_dataset_12mo.jsonl`; clf holdout spread +0.021 — selectivity real but still fails the dollar bar; full tables in SONNET_REPORT.md) |
| 8 | Microstructure features | ◐ wiring **DONE** (Phase-1 Task 2: `spread_bps`/`flow_delta_5m`/`flow_buy_frac` journal live); needs weeks of live collection before it's usable |
| 9 | Lead-lag graph features | ➖ **TESTED, NO PROMOTION** (P2.1, 2026-07-04): sector_ret_15m/peer_gap_15m ablation mixed — clf full-window spread +0.015→+0.028 but holdout +0.021→+0.007 (worse where it counts). Features journal live via the ExtraFeatures hook; revisit at a bigger dataset |
| 10 | Ensemble abstention | ❌ **KILLED** as 3-model agreement (P2.2, 2026-07-04): spread −0.014 (anti-selects) despite +$171 — the dollars came from cutting volume, not picking better. The framework caught it |
| 11 | LLM catalyst features | ⏳ future |
| 12 | Temporal CNN | ⏳ future (needs 20k+ rows) |
| 13 | Changepoint watchdog | ✅ **SHIPPED** (CUSUM w/ alarm decay in `internal/evals`; benched dip_bounce + orb_breakout on its first live day) |
| 14 | Intraday pairs | ⏳ future (needs operator decision on shorting) |
| 15 | LightGBM clf gate (margin 0.03) | ✅ **SHIPPED LIVE 2026-07-04** (operator go) — the only mechanism ever to pass the full promotion bar. Walk-forward, zero lookahead, positive accepted-vs-rejected spread AND positive dollars on all three windows: 12-mo −$718→**+$329**, Jan–Jul −$219→**+$302**, Apr–Jul holdout +$871→**+$1,512** (drawdown lower). Live stack: `ml/train_live.py` nightly (17:05 ET + boot catch-up) → per-strategy models + parity file → Go in-process scoring (`quant/clfgate.go`, leaves) with load-time Python/Go parity verification; fail-open at every failure point; every verdict logged with `signal_id` for live spread measurement. Kill switch `QUANT_CLF_GATE=false` |

> Rules of the queue (unchanged from QUANT_VISION §5): every idea states its economic
> *why* before its model, is validated walk-forward through the existing harness
> (`cmd/backtest` + `ml/train_gate.py`), is judged by pre-registered bars
> (counterfactual selectivity for signal ideas, full-window P&L + drawdown for portfolio
> ideas), and ships only by beating the incumbent. One change at a time. The current
> production setup stays as-is while these are explored.
>
> Effort: S = hours, M = ~a day, L = multi-day. Data: ✅ = already have it,
> 🕐 = needs shadow-collection time, 📥 = needs a bigger historical fetch.

---

## Tier 1 — testable this week on data we already have

### 1. First-hour cross-sectional reversal (the operator's observation, systematized)
- **Thesis:** stocks that spike in the first hour tend to fade; stocks dumped in the
  first hour tend to bounce — classic intraday overreaction + liquidity-provision
  premium (documented as intraday reversal in the cross-section).
- **How:** at 10:30 ET rank the 96-name universe by return-from-open; long the bottom
  decile *after* a stabilization trigger (first 15-min higher-low), knife-filter with
  news sentiment (already built) and the regime posture. The long-only fade of top-decile
  winners is skipped (needs shorts); track it in shadow anyway to measure the other half.
- **Validate:** new detector → 12-month replay → same bars as every strategy. Effort M, ✅.

### 2. Cross-sectional ranking gate (replace the absolute-EV gate)
- **Thesis:** predicting *which of today's signals are best* (relative) is statistically
  far easier than predicting each signal's absolute expected R — and it is exactly what
  the portfolio needs, because slots are scarce (3 concurrent). Our LightGBM edge existed
  (+0.038R spread) but was wasted by first-come slot competition; ranking converts that
  edge directly into selection.
- **How:** LightGBM LambdaRank (or pairwise logistic) trained walk-forward on day-grouped
  signals; live/backtest execution takes the top-K ranked signals per window instead of
  any signal clearing a threshold.
- **Validate:** selectivity spread by rank-bucket + portfolio P&L vs ungated. Effort M, ✅.

### 3. Time-of-day conditioning (the midday kill switch, learned properly)
- **Thesis:** measured on 6 months: 11:30–13:00 entries averaged −$3 to −$4/trade at a
  37% hit rate while 9:45–10:30 was the best window — the lunch-chop effect is real and
  strategy-dependent. Also intraday periodicity (returns at time-of-day t correlate with
  the same stock's returns at t on prior days) is a documented cross-sectional effect.
- **How:** (a) per-strategy × per-half-hour expectancy table, learned walk-forward, gates
  entries (no hand-picked windows); (b) add sin/cos time-of-day + half-hour dummies to
  every model's features; (c) test the stock-specific "same-time-yesterday" return as a
  feature.
- **Validate:** 12-month replay, per-window P&L attribution before/after. Effort S–M, ✅.

### 4. Regime mixture-of-experts (clustering applied to *days*, not stocks)
- **Thesis:** the 6-month test proved every strategy is regime-dependent (all five were
  Apr–Jun profitable, Jan–Mar toxic). A binary QQQ-vs-20d-MA brake halved drawdown but
  amputated June. The right structure is *soft routing*: learn which strategies earn in
  which market state and shift budget, don't switch the desk off.
- **How:** cluster days with a 2–3 state Gaussian HMM (or k-means as baseline) on daily
  features (QQQ trend/vol, breadth, gap size, range compression); per-state per-strategy
  expectancy from history (walk-forward); allocator weights strategies by today's
  inferred state posterior. The Strategist agent later narrates/overrides this with
  calendar knowledge (FOMC/CPI days).
- **Validate:** full-window P&L + drawdown vs (no filter | hard filter). Effort M, ✅.

### 5. Execution alpha: passive entries (when the edge is 1 R-cent, the spread IS the edge)
- **Thesis:** our per-trade gross edge is small (~$1–2); we pay ~3bps slippage + spread
  on market entries ≈ $0.6–1.2/trade. Institutions extract more from execution than from
  signals at this scale. Filling passively (limit at signal price or mid) reclaims most
  of that — at the cost of missing some winners (adverse selection: the trades that run
  away without filling you are disproportionately the good ones).
- **How:** backtester execution model v2 — limit-at-signal-price with a fill rule (fills
  if price trades ≤ limit within N minutes; else cancel or chase), measure fill-adjusted
  expectancy including the missed-winner penalty honestly.
- **Validate:** same trades, two execution models, compare net expectancy. Effort M, ✅.

### 6. Volatility-targeted sizing + rolling-Kelly throttle (consistency lever)
- **Thesis:** equal-dollar slots make P&L variance hostage to whichever volatile name
  fired; consistency (the actual goal) improves more from sizing than from entries.
- **How:** size each position so its ATR-implied daily dollar-vol contribution is equal
  (risk parity across slots); scale total gross exposure to target ~$100/day portfolio
  vol; modulate by fractional Kelly on each strategy's rolling walk-forward expectancy
  (auto-shrinks cold strategies to minimum size instead of binary demotion).
- **Validate:** same signals, sizing variants; judge by daily P&L stdev, Sharpe-like
  ratio, and worst-day — not total P&L. Effort S–M, ✅.

### 7. 12–24-month dataset + recency weighting + purged CV (feed the model properly)
- **Thesis:** the LightGBM gate flipped from harmful to useful going 4.4k → 9k samples;
  the curve is still rising. And a growing pile dilutes recent regimes — the "yesterday
  isn't today" problem — so recent samples should count more.
- **How:** fetch 12–24 months of 1-min bars (universe survivorship caveat documented),
  regenerate the dataset (~20–40k signals); train with exponential recency weights and
  purged/embargoed splits (drop training samples whose outcome windows overlap the test
  day) per López de Prado; re-run gate + ranking experiments at scale.
- **Validate:** existing bars. Effort M (mostly fetch time), 📥.

## Tier 2 — needs shadow-collection time or new plumbing

### 8. Microstructure features from the live plane (the moat history can't sell you)
- **Thesis:** at 1-minute OHLCV granularity most intraday edge is already arbitraged;
  the informative signals are in the *tape*: bid/ask spread dynamics, quote imbalance,
  buyer-vs-seller-initiated flow (the flow tracker already computes this), tape speed
  (trades/sec bursts), where price sits inside the spread. Alpaca history has none of
  this; the live SIP stream has all of it.
- **How:** extend the shadow engine's feature snapshot with spread_bps, quote imbalance,
  rolling flow delta/normalized, tape-speed z-score at signal time; collect 3–4 weeks;
  retrain the gate/ranker with feature-importance ablation (did microstructure features
  earn their spot?).
- **Validate:** selectivity spread with vs without the new features on identical rows.
  Effort S to wire, 🕐 weeks to collect.

### 9. Sector lead-lag / graph features → then a TGN if (and only if) they pay
- **Thesis:** semis move as a herd with leaders (NVDA/AVGO) and laggards; when a sector
  cluster lurches and one liquid member hasn't moved yet, the laggard tends to catch up
  (lead-lag effects at intraday horizons are well documented). This is the *right-sized*
  first step toward the STGNN/TGN ambition: build the graph signal as features, and only
  reach for a temporal graph network once the features prove there is propagation alpha
  to learn.
- **How:** rolling 1-min return correlation matrix per sector (dynamic clustering — the
  correct use of clustering on stocks); feature per signal: "peer-implied move minus
  actual move over last 15/30 min" (the catch-up gap) + sector breadth/leader state. If
  ablation shows alpha → upgrade to a small temporal GNN over the correlation graph as a
  challenger model.
- **Validate:** feature ablation in the gate/ranker first; dedicated laggard-catch-up
  detector second. Effort M then L, ✅ for features / 📥 for the deep version.

### 10. Conformal-abstention ensemble ("trade only when the models agree")
- **Thesis:** consistency comes from refusing uncertain bets. Different model families
  (LightGBM, ridge, a small MLP) disagree exactly where the data is thin; conformal
  prediction gives distribution-free intervals, so "abstain unless the whole ensemble's
  lower bound clears zero" is a principled fewer-but-better filter, not a vibe.
- **How:** train the 2–3 model families on identical walk-forward folds; conformalize
  residuals; accept a signal only when the ensemble's conformal lower bound on expected
  R > 0. Track abstention rate vs expectancy trade-off curve.
- **Validate:** selectivity + trades/day + daily P&L stdev vs single-model gate.
  Effort M, ✅.

### 11. Event/catalyst conditioning with the LLM as a *feature factory* (not a trader)
- **Thesis:** the same chart pattern means opposite things on an earnings-gap day vs a
  quiet day (post-earnings drift vs mean reversion). We already pull headlines +
  sentiment; an LLM can turn unstructured news into *structured categorical features*
  (catalyst type: earnings-beat / guidance-cut / analyst / M&A / macro / none; freshness;
  magnitude) far better than keyword matching — offline, cached, never on the hot path.
- **How:** nightly job labels each universe symbol-day with a catalyst taxonomy (forced
  tool call, cached prompts); join onto the signal dataset; retrain gate/ranker; also a
  dedicated intraday PEAD detector (earnings-beat + gap-up + first-pullback-hold → long).
- **Validate:** ablation + the PEAD detector through the standard bars. Effort M, ✅
  (history labelable retroactively via archived headlines).

### 12. Small temporal CNN on raw bar sequences (learned features as a challenger)
- **Thesis:** hand-crafted features compress away shape information (how the last 90
  minutes *look* — accumulation vs distribution). A small temporal CNN / InceptionTime on
  normalized sequences can learn shapes we didn't name — the honest, data-hungry version
  of the deep-learning ambition, and only worth running against the 20k+ row dataset.
- **How:** input = last 90 × (OHLCV + VWAP-distance + minute-of-day) normalized per-ATR;
  target = triple-barrier label; heavy regularization, purged walk-forward, ensembled
  with (never replacing) LightGBM.
- **Validate:** must beat the LightGBM champion's selectivity on the same folds to earn
  a slot in the ensemble. Effort L, 📥.

### 13. Online changepoint watchdog (self-sustaining demotion)
- **Thesis:** the 20-day scoreboard notices a broken strategy three weeks late. Bayesian
  online changepoint detection on each strategy's live trade-R stream flags distribution
  shifts within days — the "immune system" of a self-sustaining desk.
- **How:** BOCD (or simple CUSUM as baseline) per strategy on shadow/live outcomes; a
  detected regime break auto-drops the strategy to half-size/shadow pending Evaluator
  review (human-approved re-promotion).
- **Validate:** replay 6-month history — would it have benched dip_bounce before June's
  −$250? Effort S–M, ✅.

### 14. Intraday pairs / stat-arb within sector clusters (needs shorts — paper-only lab)
- **Thesis:** the purest consistency machines are market-neutral: long the laggard /
  short the leader of a cointegrated pair (MU–WDC, AMD–NVDA) mean-reverts independent of
  tape direction — exactly what our long-only book lacks in down regimes.
- **How:** rolling cointegration/distance screening within sectors; z-score entry/exit
  bands; strict per-pair risk caps. Requires enabling short legs **on the paper account
  only** — flagged as an explicit operator decision before any build.
- **Validate:** dedicated pair-backtest module (the harness needs a two-leg position
  model — the one real infra addition). Effort L, ✅ data / new sim code.

---

## Recommended sequencing

1. **Now (existing cache):** #7 big dataset → rerun gate; #2 ranking gate; #3
   time-of-day; #1 reversal detector; #6 sizing. These five compound: bigger data ×
   ranking objective × time features × sizing discipline.
2. **In parallel (zero risk):** #8 wire microstructure features into shadow (weeks of
   collection start ticking); #13 changepoint watchdog on the shadow stream.
3. **Then:** #4 regime router; #5 execution model; #11 catalyst features.
4. **Only after the dataset is 20k+ rows and the simple stack is exhausted:** #9 graph
   upgrade, #12 temporal CNN, #10 full conformal ensemble, #14 pairs lab.

Every one of these enters through the same doors: walk-forward, counterfactual
selectivity, pre-registered bars, shadow A/B, human-approved promotion.
