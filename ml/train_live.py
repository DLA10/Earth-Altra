"""Nightly production trainer for the LIVE clf entry gate (RESEARCH_BACKLOG #15).

train_gate.py answers "is this gate worth shipping?" (walk-forward evaluation). This
script is the SHIPPED half: it trains the per-strategy LightGBM classifiers on ALL
resolved signals through yesterday and exports them for the Go trader to score live
entries with (backend/internal/quant/clfgate.go).

Semantics are locked to what the walk-forward validation promoted (2026-07-04 receipts,
RESEARCH_BACKLOG #15) — same LGB_COMMON params, same win=(r>0) label, same 0.0 missing
fill, same per-strategy models, same EV rule p*rr-(1-p) applied at MARGIN=0.03 by the Go
side. The feature family per strategy is LOCKED to the keys present in the static
dataset: live journal rows may carry extra live-only columns (spread_bps, flow_*,
sector_*) — those are ignored here until an ablation promotes them.

Training data = the static backtest dataset (12-month export) + every LIVE journal
outcome from days AFTER the dataset's last day. The live journal grows daily, so the
model walks forward with reality — exactly the pattern the validation simulated.

Outputs under --outdir (backend/data/models/):
  clf_<strategy>.txt   LightGBM text model (loaded in Go via leaves)
  clf_meta.json        feature order, margin, row counts, last training day, and
                       PARITY rows: raw feature maps + this script's predicted p for
                       each — the Go loader must reproduce them or it refuses the model
  history/meta_<last_day>.json   dated archive of the meta (models are reproducible)

Run (from repo root — the backend scheduler does this nightly at ~17:05 ET):
  ml/.venv/Scripts/python.exe ml/train_live.py
"""

from __future__ import annotations

import argparse
import json
import os
import re
import warnings
from collections import defaultdict
from datetime import datetime, timezone

import numpy as np

warnings.filterwarnings("ignore")

import lightgbm as lgb

MARGIN = 0.03   # minimum EV (in R) to accept — pre-registered, matches the validation
MIN_ROWS = 150  # rows required before a strategy's model activates (fail-open below)
SEED = 42
PARITY_SAMPLES = 5

# Identical to train_gate.py — the validated configuration. Do not tune here.
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

DAY_RE = re.compile(r"^(\d{4}-\d{2}-\d{2})\.jsonl$")


def load_static(path: str) -> list[dict]:
    rows = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def load_journal(journal_dir: str, after_day: str) -> list[dict]:
    """Live journal days STRICTLY AFTER the static dataset's last day, normalized to the
    dataset row shape. Signals without a resolved outcome (or vice versa) are skipped."""
    rows: list[dict] = []
    if not os.path.isdir(journal_dir):
        return rows
    for name in sorted(os.listdir(journal_dir)):
        m = DAY_RE.match(name)
        if not m or m.group(1) <= after_day:
            continue
        day = m.group(1)
        sigs: dict[str, dict] = {}
        outs: dict[str, float] = {}
        with open(os.path.join(journal_dir, name), encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    r = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if r.get("type") == "signal" and "signal" in r:
                    s = r["signal"]
                    if s.get("id"):
                        sigs[s["id"]] = s
                elif r.get("type") == "outcome" and r.get("id"):
                    outs[r["id"]] = float(r.get("r_multiple", 0.0))
        for sid, s in sigs.items():
            if sid not in outs:
                continue
            sug = s.get("suggested") or {}
            rows.append({
                "day": day,
                "strategy": s.get("strategy", ""),
                "symbol": s.get("symbol", ""),
                "time": int(s.get("time", 0)),
                "entry": float(sug.get("entry", 0.0)),
                "stop": float(sug.get("stop", 0.0)),
                "target": float(sug.get("target", 0.0)),
                "features": s.get("features") or {},
                "r_multiple": outs[sid],
            })
    return rows


def patch_v3(path: str) -> None:
    """Relabel the model file header for the Go loader. leaves only accepts version=v3;
    LightGBM 4.x writes version=v4 with an identical on-disk structure for plain numeric
    gbdt models like ours (no linear trees, no categorical splits). The relabel is safe
    BECAUSE the Go loader's parity check is the real arbiter: if the file were misread,
    the Go predictions would not reproduce this trainer's probabilities and the model
    would be refused (fail-open)."""
    with open(path, encoding="utf-8") as f:
        txt = f.read()
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(txt.replace("version=v4\n", "version=v3\n", 1))


def feature_matrix(rows: list[dict], keys: list[str]) -> np.ndarray:
    x = np.zeros((len(rows), len(keys)))
    for i, r in enumerate(rows):
        feats = r["features"]
        for j, k in enumerate(keys):
            x[i, j] = feats.get(k, 0.0)
    return x


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", default="backend/data/ml_dataset_12mo.jsonl")
    ap.add_argument("--journal", default="backend/data/signals")
    ap.add_argument("--outdir", default="backend/data/models")
    args = ap.parse_args()

    static = load_static(args.data)
    if not static:
        raise SystemExit(f"no rows in {args.data}")
    static_last = max(r["day"] for r in static)
    live = load_journal(args.journal, static_last)
    rows = static + live
    last_day = max(r["day"] for r in rows)
    print(f"training rows: {len(static)} static (through {static_last}) + {len(live)} live journal -> {len(rows)} total, last day {last_day}")

    by_strat: dict[str, list[dict]] = defaultdict(list)
    for r in rows:
        by_strat[r["strategy"]].append(r)
    # The validated feature family per strategy: keys seen in the STATIC dataset only.
    static_keys: dict[str, set] = defaultdict(set)
    for r in static:
        static_keys[r["strategy"]].update(r["features"].keys())

    os.makedirs(args.outdir, exist_ok=True)
    meta = {
        "generated_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "last_day": last_day,
        "margin": MARGIN,
        "min_rows": MIN_ROWS,
        "strategies": {},
    }

    for strat in sorted(by_strat):
        rs = by_strat[strat]
        keys = sorted(static_keys.get(strat, set()))
        if not keys:
            print(f"  {strat:<16} SKIP: no validated feature family (strategy absent from the static dataset)")
            continue
        if len(rs) < MIN_ROWS:
            print(f"  {strat:<16} SKIP: {len(rs)} rows < {MIN_ROWS} (fail-open live)")
            continue
        y = np.array([1 if r["r_multiple"] > 0 else 0 for r in rs])
        if not (0 < y.sum() < len(y)):
            print(f"  {strat:<16} SKIP: single-class labels (fail-open live)")
            continue
        x = feature_matrix(rs, keys)
        model = lgb.LGBMClassifier(**LGB_COMMON)
        model.fit(x, y)

        fname = f"clf_{strat}.txt"
        model.booster_.save_model(os.path.join(args.outdir, fname))
        patch_v3(os.path.join(args.outdir, fname))

        # Parity rows: raw feature maps + this exact script's probability. The Go loader
        # rebuilds the vector from the map itself, so parity covers vectorization
        # (ordering + 0.0 fill) AND the tree math AND the sigmoid.
        idx = sorted({0, len(rs) // 4, len(rs) // 2, (3 * len(rs)) // 4, len(rs) - 1})[:PARITY_SAMPLES]
        probs = model.predict_proba(x[idx])[:, 1]
        parity = [{"features": rs[i]["features"], "expected_p": float(p)} for i, p in zip(idx, probs)]

        meta["strategies"][strat] = {
            "model_file": fname,
            "rows": len(rs),
            "wins": int(y.sum()),
            "feature_keys": keys,
            "parity": parity,
        }
        print(f"  {strat:<16} trained: {len(rs)} rows ({y.sum()} wins, {y.mean()*100:.1f}%), {len(keys)} features -> {fname}")

    if not meta["strategies"]:
        raise SystemExit("no strategy reached the training bar; nothing exported")

    meta_path = os.path.join(args.outdir, "clf_meta.json")
    with open(meta_path, "w", encoding="utf-8") as f:
        json.dump(meta, f, indent=1)
    hist = os.path.join(args.outdir, "history")
    os.makedirs(hist, exist_ok=True)
    with open(os.path.join(hist, f"meta_{last_day}.json"), "w", encoding="utf-8") as f:
        json.dump(meta, f, indent=1)
    print(f"meta written: {meta_path} ({len(meta['strategies'])} models, last day {last_day})")


if __name__ == "__main__":
    main()
