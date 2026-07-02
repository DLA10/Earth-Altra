"""Walk-forward LightGBM gate trainer (QUANT_VISION Phase 2).

Reads the signal dataset exported by the Go backtester (one JSONL row per published
signal: features + counterfactual bracket outcome), trains per-strategy LightGBM models
with STRICT walk-forward discipline — each day's signals are predicted by a model trained
only on rows from PRIOR days — and writes per-signal predictions for the Go backtester to
replay with (-mlpred). Also prints the selectivity report that decides promotion:
the mean actual R of accepted vs rejected signals, overall and on the June subset
(which is fully out-of-sample by construction).

Model variants:
  reg:  LGBMRegressor on r_multiple            -> pred_r directly
  clf:  LGBMClassifier on win=(r_multiple > 0) -> pred_r = p*rewardR - (1-p)*riskR (EV in R)
  rank: LGBMRanker (lambdarank, day groups)    -> relative score; combine with the Go
        replay's -mltopq causal top-quantile selection (RESEARCH_BACKLOG #2). NOTE: the
        selectivity table below splits rank scores at each strategy's global score
        median — a display approximation only; the Go replay applies the honest causal
        prior-days quantile.

--halflife N applies exponential recency weights (0.5^(age_days/N)) to training samples
(RESEARCH_BACKLOG #7: recent regimes count more). Intraday labels resolve same-day, so
cross-day leakage purging is not required — walk-forward day splits already embargo.

Usage (from repo root):
  ml/.venv/Scripts/python.exe ml/train_gate.py \
      --data backend/data/ml_dataset.jsonl --outdir backend/data --halflife 60
"""

from __future__ import annotations

import argparse
import json
import warnings
from collections import defaultdict

import numpy as np

warnings.filterwarnings("ignore")  # lightgbm is chatty about small leaves

import lightgbm as lgb

MARGIN = 0.03      # minimum predicted R to accept (same rule as the Go ridge gate)
MIN_ROWS = 150     # training rows required before a strategy's model activates
SEED = 42

LGB_COMMON = dict(
    n_estimators=150,
    learning_rate=0.05,
    num_leaves=15,
    min_child_samples=25,
    subsample=0.9,
    subsample_freq=1,
    colsample_bytree=0.9,
    reg_lambda=1.0,
    random_state=SEED,
    verbosity=-1,
)


def load_rows(path: str) -> list[dict]:
    rows = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def feature_matrix(rows: list[dict], keys: list[str]) -> np.ndarray:
    x = np.zeros((len(rows), len(keys)))
    for i, r in enumerate(rows):
        feats = r["features"]
        for j, k in enumerate(keys):
            x[i, j] = feats.get(k, 0.0)
    return x


def reward_risk(row: dict) -> float:
    """Reward:risk ratio of the signal's own bracket (for the classifier EV)."""
    risk = row["entry"] - row["stop"]
    if risk <= 0:
        return 1.0
    return (row["target"] - row["entry"]) / risk


def recency_weights(train: list[dict], halflife: float) -> np.ndarray | None:
    """0.5^(age_days/halflife) sample weights; None when disabled."""
    if halflife <= 0:
        return None
    days = sorted({r["day"] for r in train})
    idx = {d: i for i, d in enumerate(days)}
    latest = len(days) - 1
    return np.array([0.5 ** ((latest - idx[r["day"]]) / halflife) for r in train])


def rank_labels(rs: list[dict]) -> np.ndarray:
    """Graded relevance for lambdarank: bin r_multiple into 0..3."""
    return np.digitize([r["r_multiple"] for r in rs], [-0.3, 0.1, 0.8]).astype(int)


def walk_forward(rows: list[dict], variant: str, halflife: float = 0.0) -> dict[str, float]:
    """Returns {row_key: pred_r} for every row that had a trained model on its day."""
    by_strat: dict[str, list[dict]] = defaultdict(list)
    for r in rows:
        by_strat[r["strategy"]].append(r)

    preds: dict[str, float] = {}
    for strat, rs in sorted(by_strat.items()):
        rs.sort(key=lambda r: (r["day"], r["time"]))
        keys = sorted({k for r in rs for k in r["features"]})
        days = sorted({r["day"] for r in rs})
        day_rows = defaultdict(list)
        for r in rs:
            day_rows[r["day"]].append(r)

        train: list[dict] = []
        for day in days:
            todays = day_rows[day]
            if len(train) >= MIN_ROWS:
                x_tr = feature_matrix(train, keys)
                x_te = feature_matrix(todays, keys)
                w = recency_weights(train, halflife)
                p = None
                if variant == "reg":
                    y = np.array([r["r_multiple"] for r in train])
                    model = lgb.LGBMRegressor(**LGB_COMMON)
                    model.fit(x_tr, y, sample_weight=w)
                    p = model.predict(x_te)
                elif variant == "clf":
                    y = np.array([1 if r["r_multiple"] > 0 else 0 for r in train])
                    if 0 < y.sum() < len(y):
                        model = lgb.LGBMClassifier(**LGB_COMMON)
                        model.fit(x_tr, y, sample_weight=w)
                        prob = model.predict_proba(x_te)[:, 1]
                        rr = np.array([reward_risk(r) for r in todays])
                        p = prob * rr - (1 - prob) * 1.0
                else:  # rank — lambdarank over day groups (ordering objective, not value)
                    y = rank_labels(train)
                    gcount = defaultdict(int)
                    for r in train:  # train is in (day, time) order, so groups align
                        gcount[r["day"]] += 1
                    groups = [gcount[d] for d in sorted(gcount)]
                    model = lgb.LGBMRanker(**LGB_COMMON)
                    model.fit(x_tr, y, group=groups, sample_weight=w)
                    p = model.predict(x_te)
                if p is not None:
                    for r, v in zip(todays, p):
                        preds[row_key(r)] = float(v)
            train.extend(todays)
    return preds


def row_key(r: dict) -> str:
    return f"{r['strategy']}|{r['symbol']}|{r['time']}"


def report(rows: list[dict], preds: dict[str, float], label: str, split: str = "margin") -> float:
    """Prints selectivity + quintile diagnostics; returns the accepted-minus-rejected
    R spread (the promotion score). split="median" is used for rank scores (which have
    no absolute meaning) — display approximation; the Go replay is the honest judge."""
    print(f"\n──── {label} ────")
    print(f"{'strategy':<16} {'scored':>7} {'acc':>5} {'accR':>8} {'rej':>5} {'rejR':>8} {'spread':>8}")
    spreads = []
    total = {"acc": [], "rej": []}
    per_strat: dict[str, dict] = defaultdict(lambda: {"acc": [], "rej": []})
    med: dict[str, float] = {}
    if split == "median":
        by_strat = defaultdict(list)
        for r in rows:
            p = preds.get(row_key(r))
            if p is not None:
                by_strat[r["strategy"]].append(p)
        med = {s: float(np.median(v)) for s, v in by_strat.items()}
    for r in rows:
        p = preds.get(row_key(r))
        if p is None:
            continue
        cut = med.get(r["strategy"], MARGIN) if split == "median" else MARGIN
        bucket = "acc" if p >= cut else "rej"
        per_strat[r["strategy"]][bucket].append(r["r_multiple"])
        total[bucket].append(r["r_multiple"])
    for strat in sorted(per_strat):
        acc, rej = per_strat[strat]["acc"], per_strat[strat]["rej"]
        acc_r = float(np.mean(acc)) if acc else float("nan")
        rej_r = float(np.mean(rej)) if rej else float("nan")
        spread = (acc_r - rej_r) if acc and rej else float("nan")
        spreads.append(spread)
        print(f"{strat:<16} {len(acc)+len(rej):>7} {len(acc):>5} {acc_r:>+8.3f} {len(rej):>5} {rej_r:>+8.3f} {spread:>+8.3f}")
    acc, rej = total["acc"], total["rej"]
    acc_r = float(np.mean(acc)) if acc else float("nan")
    rej_r = float(np.mean(rej)) if rej else float("nan")
    overall = acc_r - rej_r if acc and rej else float("nan")
    print(f"{'TOTAL':<16} {len(acc)+len(rej):>7} {len(acc):>5} {acc_r:>+8.3f} {len(rej):>5} {rej_r:>+8.3f} {overall:>+8.3f}")

    # Quintile monotonicity: does predicted R order actual R at all?
    scored = [(preds[row_key(r)], r["r_multiple"]) for r in rows if row_key(r) in preds]
    if len(scored) >= 50:
        scored.sort(key=lambda t: t[0])
        qs = np.array_split(np.array([a for _, a in scored]), 5)
        print("quintiles (pred low→high, mean actual R): " + "  ".join(f"{np.mean(q):+.3f}" for q in qs))
    return overall


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", default="backend/data/ml_dataset.jsonl")
    ap.add_argument("--outdir", default="backend/data")
    ap.add_argument("--variants", default="reg,clf,rank")
    ap.add_argument("--halflife", type=float, default=0.0,
                    help="recency half-life in trading days (0 = unweighted)")
    ap.add_argument("--holdout", default="2026-06-02",
                    help="report a separate selectivity table for days >= this date")
    args = ap.parse_args()

    rows = load_rows(args.data)
    days = sorted({r["day"] for r in rows})
    print(f"dataset: {len(rows)} rows, {days[0]} → {days[-1]}, halflife={args.halflife}, strategies: "
          + ", ".join(sorted({r['strategy'] for r in rows})))

    holdout = [r for r in rows if r["day"] >= args.holdout]
    results = {}
    for variant in [v.strip() for v in args.variants.split(",") if v.strip()]:
        preds = walk_forward(rows, variant, args.halflife)
        split = "median" if variant == "rank" else "margin"
        print(f"\n════════ LightGBM {variant} — walk-forward, {len(preds)} scored signals ════════")
        results[variant] = {
            "overall": report(rows, preds, "FULL WINDOW", split),
            "holdout": report(holdout, preds, f"HOLDOUT ≥ {args.holdout} (OOS by construction)", split),
            "preds": preds,
        }

    for variant, res in results.items():
        out = f"{args.outdir}/ml_predictions_{variant}.jsonl"
        with open(out, "w", encoding="utf-8") as f:
            for k, v in res["preds"].items():
                strat, sym, t = k.split("|")
                f.write(json.dumps({"strategy": strat, "symbol": sym, "time": int(t), "pred_r": v}) + "\n")
        print(f"predictions written: {out}")

    print("\nPromotion bar: accepted-R must exceed rejected-R (positive spread), overall AND")
    print("in the holdout, and the Go replay must confirm in dollars, before any gate")
    print("touches order flow.")


if __name__ == "__main__":
    main()
