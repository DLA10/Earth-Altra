"""Nightly research loop (QUANT_VISION §5, LangGraph orchestration).

A LangGraph StateGraph runs the offline improvement cycle — deliberately OFF the trading
hot path (which stays raw Go + forced tool calls):

    collect ──► evaluate ──► propose ──► [HUMAN GATE] ──► apply
    (read journals,  (deterministic   (Opus drafts ≤3   (interrupt_before:    (append approved
     decisions,       day stats, no    evidence-cited     approve in the CLI,   changes to the
     scoreboard)      LLM)             proposals via      auto-pend when        audit trail)
                                       forced tool call)  non-interactive)

Nothing here places orders or edits code. Approved proposals land in
backend/data/evals/approved_changes.jsonl as explicit change instructions for the
operator (or a future config-applier) — evolution by evidence, gated by a human.

Run (from repo root, evenings):
  ml/.venv/Scripts/python.exe ml/research_loop.py            # yesterday's trading day
  ml/.venv/Scripts/python.exe ml/research_loop.py --day 2026-07-02
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from collections import defaultdict
from pathlib import Path
from typing import TypedDict

from langgraph.checkpoint.memory import MemorySaver
from langgraph.graph import END, StateGraph

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "backend" / "data"


def env_key() -> str:
    """Read ANTHROPIC_API_KEY from the backend .env (never committed)."""
    if k := os.environ.get("ANTHROPIC_API_KEY"):
        return k
    envfile = ROOT / "backend" / ".env"
    if envfile.exists():
        for line in envfile.read_text(encoding="utf-8").splitlines():
            if line.strip().startswith("ANTHROPIC_API_KEY="):
                return line.split("=", 1)[1].strip()
    return ""


class LoopState(TypedDict, total=False):
    day: str
    digest: dict
    proposals: list
    approved: list
    status: str


# ---------- nodes ----------

def collect(state: LoopState) -> LoopState:
    """Gather the day's raw records: signal journal, decision log, scoreboard."""
    day = state["day"]
    out = {"signals": [], "outcomes": [], "decisions": [], "scoreboard": None}
    sig = DATA / "signals" / f"{day}.jsonl"
    if sig.exists():
        for line in sig.read_text(encoding="utf-8").splitlines():
            if not line:
                continue
            r = json.loads(line)
            out["signals" if r.get("type") == "signal" else "outcomes"].append(r)
    dec = DATA / "decisions" / f"{day}.jsonl"
    if dec.exists():
        out["decisions"] = [json.loads(l) for l in dec.read_text(encoding="utf-8").splitlines() if l]
    sb = DATA / "evals" / "scoreboard.json"
    if sb.exists():
        out["scoreboard"] = json.loads(sb.read_text(encoding="utf-8"))
    return {"digest": {"raw": out}}


def evaluate(state: LoopState) -> LoopState:
    """Deterministic day stats — numbers come from code, never from the model."""
    raw = state["digest"]["raw"]
    per = defaultdict(lambda: {"signals": 0, "outcomes": 0, "sum_r": 0.0, "stops": 0, "targets": 0})
    for s in raw["signals"]:
        per[s["signal"]["strategy"]]["signals"] += 1
    for o in raw["outcomes"]:
        p = per[o["strategy"]]
        p["outcomes"] += 1
        p["sum_r"] += o.get("r_multiple", 0)
        if o.get("exit_reason") == "stop":
            p["stops"] += 1
        elif o.get("exit_reason") == "target":
            p["targets"] += 1
    strategies = {
        k: {**v, "mean_r": round(v["sum_r"] / v["outcomes"], 3) if v["outcomes"] else 0.0}
        for k, v in sorted(per.items())
    }
    agents = defaultdict(int)
    trades, skips = [], defaultdict(int)
    for d in raw["decisions"]:
        agents[f'{d.get("agent")}:{d.get("event")}'] += 1
        if d.get("agent") == "signal_trader" and d.get("event") == "order":
            trades.append(d.get("note", ""))
        if d.get("agent") == "signal_trader" and d.get("event") == "skip":
            reason = d.get("note", "").split(": ", 1)[-1][:60]
            skips[reason] += 1
    digest = {
        "day": state["day"],
        "per_strategy": strategies,
        "agent_activity": dict(agents),
        "paper_entries": trades,
        "skip_reasons": dict(sorted(skips.items(), key=lambda kv: -kv[1])[:8]),
        "scoreboard": raw["scoreboard"],
    }
    print(f"[evaluate] {state['day']}: {sum(v['signals'] for v in strategies.values())} signals, "
          f"{sum(v['outcomes'] for v in strategies.values())} outcomes, {len(trades)} paper entries")
    return {"digest": digest}


PROPOSAL_SCHEMA = {
    "type": "object",
    "properties": {
        "proposals": {
            "type": "array",
            "maxItems": 3,
            "items": {
                "type": "object",
                "properties": {
                    "target": {"type": "string", "description": "the knob/rule to change (e.g. QUANT_TRAIL_PCT, a strategy filter, a TOD bucket)"},
                    "change": {"type": "string", "description": "the specific change, with values"},
                    "evidence": {"type": "string", "description": "the numbers from the digest that justify it"},
                    "expected_effect": {"type": "string"},
                },
                "required": ["target", "change", "evidence", "expected_effect"],
            },
        },
        "no_change_reason": {"type": "string", "description": "if proposing nothing, why stability is right today"},
    },
    "required": ["proposals"],
}

REVIEWER_SYSTEM = """You are the post-market research reviewer for an intraday quant desk
(long-only, paper account, consistency over peak profits). You receive a deterministic digest of
one trading day: per-strategy signal/outcome stats, agent activity, actual paper entries, skip
reasons, and the rolling scoreboard. Propose AT MOST 3 specific, evidence-cited parameter or rule
changes that move the desk toward CONSISTENT profitability — or propose NOTHING if the day doesn't
justify change (stability is part of consistency; one red day is rarely evidence). Every proposal
must cite numbers from the digest, name a concrete target knob, and will be reviewed by a human
before anything is applied. Never propose changes to live-money code, the dip-watcher alerts, or
risk caps upward. Call record_proposals."""


def propose(state: LoopState) -> LoopState:
    key = env_key()
    if not key:
        print("[propose] no ANTHROPIC_API_KEY — skipping proposals")
        return {"proposals": [], "status": "no_key"}
    import anthropic

    client = anthropic.Anthropic(api_key=key)
    msg = client.messages.create(
        model=os.environ.get("QUANT_REVIEW_MODEL", "claude-opus-4-8"),
        max_tokens=1500,
        system=REVIEWER_SYSTEM,
        tools=[{"name": "record_proposals", "description": "Record the evidence-cited change proposals.",
                "input_schema": PROPOSAL_SCHEMA}],
        tool_choice={"type": "tool", "name": "record_proposals"},
        messages=[{"role": "user", "content": json.dumps(state["digest"])}],
    )
    proposals = []
    for block in msg.content:
        if block.type == "tool_use" and block.name == "record_proposals":
            proposals = block.input.get("proposals", [])
            if not proposals and block.input.get("no_change_reason"):
                print(f"[propose] no changes proposed: {block.input['no_change_reason']}")
    print(f"[propose] {len(proposals)} proposal(s)")
    for i, p in enumerate(proposals, 1):
        print(f"  {i}. [{p['target']}] {p['change']}\n     evidence: {p['evidence']}")
    return {"proposals": proposals}


def apply_node(state: LoopState) -> LoopState:
    """Audit-trail the human's verdicts. Approved changes are instructions for the
    operator (or a future config applier) — nothing is auto-edited."""
    day = state["day"]
    outdir = DATA / "evals"
    outdir.mkdir(parents=True, exist_ok=True)
    approved = state.get("approved") or []
    pending = [p for p in state.get("proposals", []) if p not in approved]
    if approved:
        with open(outdir / "approved_changes.jsonl", "a", encoding="utf-8") as f:
            for p in approved:
                f.write(json.dumps({"day": day, **p, "status": "approved"}) + "\n")
    if pending:
        (outdir / f"proposals_{day}.json").write_text(
            json.dumps({"day": day, "status": "pending_review", "proposals": pending}, indent=1),
            encoding="utf-8")
    print(f"[apply] {len(approved)} approved (audit trail), {len(pending)} pending in proposals_{day}.json")
    return {"status": "done"}


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--day", default=None, help="trading day (default: latest journal)")
    args = ap.parse_args()

    day = args.day
    if not day:
        journals = sorted((DATA / "signals").glob("*.jsonl"))
        if not journals:
            print("no signal journals yet")
            return
        day = journals[-1].stem

    g = StateGraph(LoopState)
    g.add_node("collect", collect)
    g.add_node("evaluate", evaluate)
    g.add_node("propose", propose)
    g.add_node("apply", apply_node)
    g.set_entry_point("collect")
    g.add_edge("collect", "evaluate")
    g.add_edge("evaluate", "propose")
    g.add_edge("propose", "apply")
    g.add_edge("apply", END)

    # Human-in-the-loop: the graph checkpoints and INTERRUPTS before `apply`; the human
    # approves per-proposal in the CLI, then the graph resumes with the verdicts.
    graph = g.compile(checkpointer=MemorySaver(), interrupt_before=["apply"])
    cfg = {"configurable": {"thread_id": day}}

    graph.invoke({"day": day}, cfg)  # runs collect → evaluate → propose, then pauses

    snapshot = graph.get_state(cfg)
    proposals = snapshot.values.get("proposals", [])
    approved: list = []
    if proposals and sys.stdin.isatty():
        print("\n── HUMAN GATE — approve proposals? ──")
        for i, p in enumerate(proposals, 1):
            ans = input(f"approve #{i} [{p['target']}]? (y/N) ").strip().lower()
            if ans == "y":
                approved.append(p)
    elif proposals:
        print("\n(non-interactive: all proposals parked as pending for review)")

    graph.update_state(cfg, {"approved": approved})
    graph.invoke(None, cfg)  # resume: apply


if __name__ == "__main__":
    main()
