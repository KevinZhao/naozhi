"""
AQ4: does a user answer sent as a plain user message correctly resume CC's
train of thought?

Setup:
  1. Fresh CC session.
  2. Send ambiguous prompt → observe AskUserQuestion tool_use + auto-error tool_result + bail text + result.
  3. Send a second user message formatted like our planned card-answer payload
     ("Error style: Return an error. Target: I'll paste the code.") and see if
     CC picks up where it left off (knows it's an answer, keeps going).

This validates the "just send the answer as a normal user message" path.

Budget: 1 CLI spawn, 2 user turns.
"""
from __future__ import annotations
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "passthrough"))
from harness import Session  # noqa: E402

OUT = os.path.join(HERE, "out")
os.makedirs(OUT, exist_ok=True)


def wait_for_result(s: Session, expected: int, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if s.result_count() >= expected:
            return True
        time.sleep(0.3)
    return False


def summarize_turn(s: Session, turn_idx: int) -> None:
    """Print events from this turn only."""
    print(f"\n---- turn #{turn_idx} events (stdout) ----")
    # Find the turn boundary: we printed marker in send_raw; use results as sentinels.
    results_seen = 0
    for (ts, lbl, line) in s.lines:
        if lbl != "stdout":
            continue
        try:
            j = json.loads(line)
        except Exception:
            continue
        t = j.get("type")
        if t == "result":
            results_seen += 1
            if results_seen == turn_idx:
                st = j.get("subtype")
                stop = j.get("stop_reason")
                rtxt = (j.get("result") or "")[:200]
                print(f"  t+{ts:5.2f}s  result         subtype={st} stop={stop} result={rtxt!r}")
                break
        # only print lines within the target turn
        if results_seen != turn_idx - 1:
            continue
        summary = ""
        if t == "assistant":
            for c in j.get("message", {}).get("content", []):
                ct = c.get("type")
                if ct == "thinking":
                    summary += f"[thinking:{c.get('thinking','')[:100]!r}] "
                elif ct == "tool_use":
                    inp = c.get("input", {})
                    summary += f"[tool_use:{c.get('name')} id={c.get('id','')[-12:]}] "
                elif ct == "text":
                    summary += f"[text:{c.get('text','')[:120]!r}] "
        elif t == "user":
            for c in j.get("message", {}).get("content", []):
                ct = c.get("type")
                if ct == "tool_result":
                    err = "(is_error)" if c.get("is_error") else ""
                    content = c.get("content", "")
                    if isinstance(content, str):
                        summary += f"[tool_result{err} tuid={c.get('tool_use_id','')[-12:]}: {content[:60]!r}] "
        elif t == "system":
            st = j.get("subtype", "")
            summary = f"subtype={st}"
        if summary:
            print(f"  t+{ts:5.2f}s  {t:12s} {summary}")


def run() -> int:
    s = Session(label="aq4", echo=False)

    # ---- Turn 1: ambiguous prompt, expect AskUserQuestion + auto-error + bail text ----
    prompt1 = (
        "I want to add error handling to a function. I'm not sure whether to "
        "use panic, return an error, or log-and-continue. Before doing anything, "
        "use the AskUserQuestion tool to ask me which approach I prefer. Do not "
        "guess. Do not write code yet."
    )
    print("[driver] sending turn #1 — ambiguous prompt")
    s.send(prompt1)
    if not wait_for_result(s, 1, 45):
        print("[!] turn 1 did not finish within 45s")
        s.close(wait=3)
        return 1
    summarize_turn(s, 1)

    # ---- Turn 2: user answers as if they picked options from our card ----
    # This is the format naozhi would compose client-side from AskUserQuestion.input.
    answer_text = (
        "Error style: Return an error. "
        "Target: I'll paste the code — here it is:\n\n"
        "```go\n"
        "func LoadConfig(path string) Config { ... }\n"
        "```"
    )
    print("\n[driver] sending turn #2 — user answer payload")
    s.send(answer_text)
    if not wait_for_result(s, 2, 60):
        print("[!] turn 2 did not finish within 60s")
    summarize_turn(s, 2)

    # Dump full log regardless
    dump_path = os.path.join(OUT, "aq4_full.log")
    s.dump(dump_path)
    print(f"\n[driver] full log -> {dump_path}")

    # Heuristic check: did CC treat turn 2 as picking up the AQ thread?
    # Signals:
    #   - turn 2 thinking mentions "answer" / "chose" / "error" / "return"
    #   - turn 2 text output doesn't re-ask, proceeds to write code or explain
    turn2_text_blocks = []
    results_seen = 0
    for (_, lbl, line) in s.lines:
        if lbl != "stdout":
            continue
        try:
            j = json.loads(line)
        except Exception:
            continue
        if j.get("type") == "result":
            results_seen += 1
            continue
        if results_seen != 1:
            continue
        if j.get("type") == "assistant":
            for c in j.get("message", {}).get("content", []):
                if c.get("type") == "text":
                    turn2_text_blocks.append(c.get("text", ""))
                elif c.get("type") == "thinking":
                    turn2_text_blocks.append("[thinking] " + c.get("thinking", ""))

    joined = " ".join(turn2_text_blocks).lower()
    print("\n========== AQ4 HEURISTIC CHECK ==========")
    print(f"  turn 2 text/thinking length: {len(joined)} chars")
    positive_signals = [w for w in ["return an error", "return error", "chosen", "you chose", "your choice", "error handling", "loadconfig"] if w in joined]
    negative_signals = [w for w in ["which approach", "which would you prefer", "tell me more", "please specify"] if w in joined]
    print(f"  positive signals: {positive_signals}")
    print(f"  negative signals: {negative_signals}")
    verdict = "OK — CC seems to continue the thread" if positive_signals and not negative_signals else ("WARN — CC may not have recognized answer" if negative_signals else "INCONCLUSIVE")
    print(f"  verdict: {verdict}")

    s.close(wait=3)
    return 0


if __name__ == "__main__":
    sys.exit(run())
