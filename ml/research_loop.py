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


def env_get(name: str) -> str:
    """Read a variable from the environment or the backend .env (never committed)."""
    if v := os.environ.get(name):
        return v
    envfile = ROOT / "backend" / ".env"
    if envfile.exists():
        for line in envfile.read_text(encoding="utf-8").splitlines():
            if line.strip().startswith(name + "="):
                return line.split("=", 1)[1].strip()
    return ""


def env_key() -> str:
    return env_get("ANTHROPIC_API_KEY")


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
    symbols_seen = {s["signal"].get("symbol") for s in raw["signals"]}
    import datetime
    digest = {
        "day": state["day"],
        "as_of": datetime.datetime.now().astimezone().isoformat(timespec="minutes"),
        "universe_symbols_with_signals": sorted(x for x in symbols_seen if x),
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

REVIEWER_SYSTEM = """You are the research reviewer for an intraday quant desk (long-only, paper
account, consistency over peak profits). You receive a deterministic digest: per-strategy
signal/outcome stats across the whole universe, agent activity, actual paper entries, skip
reasons, and the rolling scoreboard.

YOUR DEFAULT OUTPUT IS ZERO PROPOSALS. Stability is part of consistency, and the deterministic
layers (time-of-day gate, scoreboard demotion, CUSUM watchdog, loss cap) already self-correct —
most problems fix themselves without a rule change. Propose a change ONLY when the evidence bar
is met, ALL of: (1) the SAME failure pattern appears across ≥3 trading days OR ≥30 outcomes for
that specific claim — a single session is NEVER sufficient evidence, whatever happened; (2) no
existing automatic mechanism already handles it; (3) you can cite the exact numbers. If the
digest's as_of is mid-session (before the 16:00 close), the day is incomplete — treat it as an
informational pulse and hold the bar even higher. When you propose nothing, say why in
no_change_reason (one short plain-English paragraph a novice can read).

Hard limits: at most 3 proposals; never touch live-money code, the dip-watcher alerts, risk caps
upward, or pre-registered eval constants. Every proposal is applied by a HUMAN, later, manually.
Call record_proposals."""


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


def notify_telegram(state: LoopState) -> None:
    """Send the report to the operator's Telegram (same bot as the dip watcher)."""
    token, chat = env_get("TELEGRAM_BOT_TOKEN"), env_get("TELEGRAM_CHAT_ID")
    if not token or not chat:
        print("[notify] telegram not configured — skipping")
        return
    d = state.get("digest", {})
    per = d.get("per_strategy", {})
    lines = [f"🧪 Research loop — {d.get('day')} (as of {d.get('as_of', '')[-11:-6]})"]
    tot_sig = sum(v.get("signals", 0) for v in per.values())
    tot_out = sum(v.get("outcomes", 0) for v in per.values())
    lines.append(f"signals {tot_sig} · outcomes {tot_out} · paper entries {len(d.get('paper_entries', []))} · symbols {len(d.get('universe_symbols_with_signals', []))}")
    for k, v in per.items():
        if v.get("outcomes"):
            lines.append(f"• {k}: {v['outcomes']} outcomes, mean R {v['mean_r']:+.2f} ({v['targets']}T/{v['stops']}S)")
    sb = d.get("scoreboard") or {}
    if dem := sb.get("demoted_set"):
        lines.append("⛔ demoted: " + ", ".join(dem))
    props = state.get("proposals") or []
    if props:
        lines.append(f"\n📋 {len(props)} proposed change(s) — YOUR call, nothing auto-applies:")
        for i, p in enumerate(props, 1):
            lines.append(f"{i}. [{p['target']}] {p['change']}\n   why: {p['evidence'][:200]}")
        lines.append("(full details: backend/data/evals/proposals_%s.json)" % d.get("day"))
    else:
        lines.append("\n✅ no changes proposed — system judged today's evidence insufficient for tinkering")
    text = "\n".join(lines)[:3800]

    import urllib.request
    req = urllib.request.Request(
        f"https://api.telegram.org/bot{token}/sendMessage",
        data=json.dumps({"chat_id": chat, "text": text}).encode(),
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            print(f"[notify] telegram: {resp.status}")
    except Exception as e:  # noqa: BLE001 — never let notification kill the loop
        print(f"[notify] telegram failed: {e}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--day", default=None, help="trading day (default: latest journal)")
    ap.add_argument("--notify", action="store_true", help="send the report to Telegram")
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

    if args.notify:
        notify_telegram(graph.get_state(cfg).values)


if __name__ == "__main__":
    main()
