# Instruction.md — Pre-market universe selection (Option B)

> **How to use:** Start a fresh Claude Code session **before the US market opens** and say
> *"Run Instruction.md"* (or "do the pre-market analysis"). Claude reads this file and produces
> today's structured trading universe + market posture, writes it to
> `backend/data/daily_universe.json`, and the backend ingests it on its next read. This is the
> human/Claude-curated stock-selection layer — there is **no automated research agent yet**
> (deferred until the entry+exit agents are proven by evals).

---

## 0. North star (applies to every decision, every agent)

**Consistency over peak profits.** We are building an AI quant team that wins by being
*repeatable and risk-controlled*, not by hitting home runs. Therefore the universe we hand the
agents must be **liquid, orderly, "in play," and dip-friendly** — names that pull back and
recover in a tradeable way — not lottery tickets. **When in doubt, leave it out.** A smaller,
higher-quality universe beats a big noisy one.

The downstream strategy is **dip → bounce**: the detector waits for an oversold, below-VWAP
pullback (≥ ~0.5× daily ATR off the high) on an in-play name, confirmed by a green 5-min candle.
So we want stocks that **actually dip intraday and recover**, with enough volatility to make the
dip worth trading but enough order that it isn't a falling knife.

---

## 1. What makes a GOOD candidate (the inclusion filter)

A symbol earns a place in today's universe only if it clears ALL of these:

- **Liquid.** Avg daily volume ≥ ~2M shares; tight typical spread (large-cap / liquid mid-cap).
  Illiquid = bad fills, gappy dips, unreliable signals. **Hard requirement.**
- **Right price band.** Roughly **$15–$600**. Too cheap = penny-stock noise; too expensive =
  awkward sizing on $2k/position. (Fractionable names relax the high end.)
- **"In play" today.** A real reason it will move and attract volume: a catalyst (below),
  elevated pre-market volume / RVOL, or it's a current sector leader. A stock with no reason to
  move just chops around VWAP and produces fake signals.
- **Orderly volatility.** Daily ATR roughly **1.5%–6%** of price — enough range that a dip is
  worth trading, not so wild it's untradeable. Prefer names that *trend then pull back*, not
  ones that spike and collapse.
- **Tradable on Alpaca.** Tradable + (ideally) fractionable; common US equity, not a thin ADR,
  not a leveraged ETF, not a recent IPO with no history.

## 2. What to AVOID / EXCLUDE (the kill list)

Exclude — and log the reason — if any apply:

- **Earnings within ~2 trading days** (gap/binary risk) → exclude, or tag `earnings_soon` and
  let the agents stand down on it. Check the **earnings calendar**.
- **Pending dilution / offering:** a just-announced **secondary offering, ATM, or S-1** (SEC
  filings) → exclude. These bleed all day; dips don't recover.
- **Binary event risk:** FDA/PDUFA dates, major legal rulings, M&A votes → exclude unless you
  explicitly want it and flag it.
- **Halt-prone / extreme gappers:** opening gap > ~15% on news, or low-float squeeze names →
  exclude (knife risk, halts).
- **Macro risk days:** FOMC/CPI/NFP/jobs print mornings → set **market posture = cautious or
  stand-down** for the day (don't fight a coin-flip tape). Check the **economic calendar**.
- **Thin / wide-spread names**, sub-$2M volume, OTC, recent IPOs (<3 months) → exclude.

## 3. Data sources to check (A→Z each morning)

Use WebSearch/WebFetch and the backend's own endpoints. Cover:

1. **Market regime first.** SPY & QQQ pre-market direction, **VIX** level/trend, overnight
   futures, any overnight macro shock. This sets the day's **posture** (§5).
2. **Economic calendar** — is today FOMC / CPI / PPI / NFP / Fed-speak? If yes → cautious.
3. **Earnings calendar** — who reports today (avoid intraday) and who reported *this morning*
   (post-earnings names can be great in-play candidates if liquid and not gapping insanely).
4. **Pre-market movers / gappers** — top % gainers & losers on volume (also `/api/movers`).
   These are the most "in play." Vet each against §1/§2.
5. **News & catalysts** — analyst upgrades/downgrades, guidance, product/partnership news,
   sector themes (AI, semis, etc.). Tie each candidate to a concrete *why*.
6. **SEC filings (EDGAR full-text search):**
   - **8-K** — material events (good or bad; offering = avoid, big contract = interesting).
   - **Form 4** — **insider buys** (cluster buying by execs = bullish signal worth a tag).
   - **SC 13D / 13G** — **activist / whale stakes** taken in a name.
   - **13F** (quarterly) — notable fund positioning; slower signal, use for thematic bias.
   - **S-1 / 424B / ATM** — dilution → **avoid**.
7. **Sector rotation** — which sectors are bid today; bias the universe toward leaders.
8. **The existing watchlist** (`EVENT_DRIVEN_WATCHLIST.md` / `/api/watchlist/symbols`) — start
   here, then add/drop based on today's reads.

## 4. Selection process (steps)

1. Read this file + the current watchlist + recent `daily_universe.json` (yesterday's, for
   continuity) + the latest entry in the eval/decision log (what worked recently).
2. Assess **market regime** → set posture (§5).
3. Pull movers/catalysts/filings/calendars (§3); build a candidate long-list.
4. Apply the inclusion filter (§1) and kill list (§2) to each.
5. Rank by quality for the **dip-bounce, consistency** goal. Keep **~10–20 names** (Tier-1 core
   + Tier-2 watch). Do **not** dump 50+ names — focus beats breadth.
6. Assign each a `sentiment_lean` (your read; the live Agent 4 will refine intraday) and any
   `risk_flags`.
7. Write the structured output (§6). State your reasoning briefly so the log is reviewable.
8. **Push every universe symbol to the backend watchlist** (CRITICAL — see §6a). The dip detector
   only scans watchlist symbols, so a universe name that isn't on the watchlist is never traded.

## 5. Market posture (gates how aggressive the agents are)

- **`normal`** — clean tape, no macro landmines. Agents trade normally.
- **`cautious`** — macro print today, choppy/indecisive tape, or high VIX. Fewer/cleaner setups
  only; tighten exits. (Backend can raise the dip-quality bar / lower max trades.)
- **`stand_down`** — major shock or FOMC hour, VIX spiking. No new entries; manage exits only.

Always include a one-line rationale for the posture.

## 6. OUTPUT — structured, machine-readable (the contract)

Write **`backend/data/daily_universe.json`** in exactly this shape (the backend ingests it; keep
keys stable):

```json
{
  "date": "2026-06-30",
  "generated_at_et": "08:55",
  "market_regime": {
    "posture": "normal | cautious | stand_down",
    "spy_bias": "up | down | flat",
    "qqq_bias": "up | down | flat",
    "vix": 14.2,
    "macro_today": "none | FOMC | CPI | NFP | ...",
    "notes": "one-line rationale"
  },
  "allocation": {
    "budget_usd": 8000,
    "per_position_usd": 2000,
    "max_concurrent": 3,
    "notes": "ONE shared budget for the whole team; keep budget_usd <= the paper account's CASH (~$8k) so the team allocates real, not margin, capital. Allocator funds highest dip-quality first."
  },
  "universe": [
    {
      "symbol": "NVDA",
      "tier": 1,
      "catalyst": "AI demand upgrade from <bank>",
      "sentiment_lean": "positive | neutral | negative",
      "risk_flags": [],
      "notes": "liquid, orderly, leader"
    }
  ],
  "excluded": [
    { "symbol": "XYZ", "reason": "secondary offering announced (8-K)" }
  ]
}
```

Rules: `universe` = 10–20 entries; every entry must have a real `catalyst` or be a proven
liquid leader; `risk_flags` from a fixed vocab (`earnings_soon`, `gap_up`, `gap_down`,
`thin_premarket`, `macro_day`, `insider_buy`, `activist_stake`). Anything dropped goes in
`excluded` **with a reason** (so selection is auditable — never silently drop).

Also: use today's actual ET date for `date` (the example is illustrative). Keep
`budget_usd ≤ the paper account's CASH (~$8k)`; if you want `max_concurrent` positions to all be
fundable, set `per_position_usd ≈ budget_usd / max_concurrent` (e.g. $1,000 × 3), otherwise the
allocator simply funds fewer at the larger size — both are valid, just be intentional.

## 6a. Make the universe tradeable (REQUIRED — easy to miss)

Writing `daily_universe.json` only tells the agents which symbols they're *allowed* to trade. The
**dip detector that triggers them scans only the backend watchlist**, so a universe symbol that
isn't on the watchlist produces no dip signal and is **never traded** (it's silently ignored). So
after writing the file, for **every** symbol in `universe`, ensure it is on the watchlist:

- Read the current watchlist: `GET /api/watchlist/symbols`.
- For each universe symbol not already present, add it:
  `POST /api/watchlist/symbols` with body `{"symbol":"NVDA"}`.
  (This backfills history, starts the live SIP stream, AND registers it with the dip detector. It
  persists across restarts. The backend must be running — start it first if it isn't.)
- You do NOT need to remove last day's extra watchlist names — the dip bot still alerts on them,
  but the agents won't trade them (the universe gate handles that). Adding is what matters.

## 7. Logging & auditability

- This daily file IS the selection log (date-stamped; keep history — don't overwrite blindly,
  archive prior days if practical).
- **Agent decisions** (entry/exit) are logged separately by the backend as structured JSONL
  (one line per decision: timestamp, symbol, agent, full input snapshot, the model's structured
  output `{action, confidence, reason}`, and the resulting order + later outcome). That log is
  what the post-market review reads to measure **consistency**.
- When you run this file, end by noting (in chat) the posture, the count, and the 2–3 names you
  felt most/least sure about — so there's a human-readable trail too.

## 8. Review prior reports & evolve (do this FIRST each session)

Before building today's universe, **read and learn from history**:
- The most recent **post-market review reports** (`backend/data/reviews/`) — they are recorded
  daily, one structured report per trading day.
- The **agent decision logs** (`backend/data/decisions/*.jsonl`) — every entry/exit decision,
  its input snapshot, the structured output, and the outcome.

Use them to (a) shape today's universe — drop setups/symbols that consistently lose, favor what
repeatably works; and (b) **propose parameter/rule modifications** as the days go by — but ONLY
changes that demonstrably move us toward **consistent profits**. Every proposed change must:
- cite specific evidence from the logs/reviews (e.g. "stop too tight — 6 of 9 losers were
  shaken out then recovered → widen stop"; "Agent 2 over-trading sub-1.3 RVOL dips, 70% losers
  → raise RVOL floor"),
- be **surfaced to the user for approval before applying** (we evolve deliberately, never
  randomly), and
- be logged itself, so its effect can be measured afterward.

Never tinker for its own sake. Stability is part of consistency — change one thing at a time and
watch the next review.

## 9. Shared budget allocation (model real-life capital sharing)

Everything runs in **ONE paper account with ONE shared budget** — deliberately, because in real
life the team must *share and allocate finite capital*, not give each agent its own infinite
wallet. A **deterministic allocator (in Go, not an LLM)** owns the money:
- **Total budget** and **per-position size** and **max concurrent positions** come from the
  `allocation` block you set in `daily_universe.json` (§6).
- **Max concurrent** is a hard cap (start 2–3): it bounds both risk and how much information
  Agent 3 must juggle at once.
- When **multiple dips compete** for limited capital, the allocator funds the **highest
  dip-quality first** (Agent 2's confidence × deterministic factors: RVOL, depth×ATR, sentiment
  lean, universe tier). Lower-ranked dips are **skipped and logged**, never force-funded.
- Set `budget_usd`, `per_position_usd`, and `max_concurrent` based on your pre-market read
  (tighter on `cautious` days, near-zero on `stand_down`).

## 10. Reminders

- This is **paper trading** — let the agents act without human override so we measure true
  ability. Your job here is only to hand them a clean, high-quality universe.
- Data sources are real even pre-market; live ticks start at the open.
- **The dip detector only scans backend-watchlist symbols** — so every universe symbol MUST be
  added to the watchlist via `POST /api/watchlist/symbols` (§6a), or the agents never see a dip for
  it and silently never trade it. (Note: the "streams any symbol on demand" behaviour is for
  browser charts only — it does NOT feed the dip detector.)
- **Consistency over peak profit** — bias every borderline call toward *skip it*.
