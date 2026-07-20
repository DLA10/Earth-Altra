"""RBT scan universe — the SINGLE source of truth for the trainer and the live scorer.

200 plan (2026-07-20): legacy 100-name list UNION the curated liquid baseline
(QUANT_UNIVERSE.baseline-2026-07-16.json, ~160 liquid shortable large caps). The CURRENT
QUANT_UNIVERSE.json (534-name throughput expansion) is deliberately NOT used here: RBT
shorts, so thin/hard-to-borrow names produce misleading paper fills, and +500 names would
drown the pairwise cointegration screen in chance "families" (at p<0.10, ~1 in 10 random
pairs passes; the test count must stay small enough for that to mean something).

Must stay aligned with the Go-side union in backend/cmd/server/main.go (same baseline file,
same legacy list). RBT_UNIVERSE_PATH overrides the baseline file location.
"""
import os
import json

LEGACY_100 = [
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
    "CAT", "DE", "HON", "EMR", "ETN", "GE", "ITW", "PH", "ROK", "PWR",
]


def _baseline_symbols():
    here = os.path.dirname(os.path.abspath(__file__))
    candidates = [
        os.getenv("RBT_UNIVERSE_PATH", ""),
        os.path.join(here, "..", "QUANT_UNIVERSE.baseline-2026-07-16.json"),
    ]
    for p in candidates:
        if p and os.path.exists(p):
            try:
                with open(p) as f:
                    d = json.load(f)
                out = []
                for members in (d.get("sectors") or {}).values():
                    out.extend(members)
                return out
            except Exception as e:  # a broken file must not kill the nightly retrain
                print(f"rbt_universe: baseline file unreadable ({e}) — legacy list only")
                return []
    print("rbt_universe: baseline file not found — legacy list only")
    return []


UNIVERSE = sorted(set(LEGACY_100) | set(_baseline_symbols()))

if __name__ == "__main__":
    print(f"{len(UNIVERSE)} symbols ({len(LEGACY_100)} legacy + baseline union)")
