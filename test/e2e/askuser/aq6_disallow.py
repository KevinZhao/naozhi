"""
AQ6: can we disable AskUserQuestion via --disallowedTools?

If yes, operator / per-session config can opt out entirely — CC then has to
make its own decision or ask in plain text. This gives us an escape hatch.

Budget: 1 CLI spawn, 1 prompt.
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


def wait_for_result(s: Session, n: int, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if s.result_count() >= n:
            return True
        time.sleep(0.3)
    return False


def run() -> int:
    prompt = (
        "I want to add error handling to a function. I'm not sure whether to "
        "use panic, return an error, or log-and-continue. Before doing anything, "
        "use the AskUserQuestion tool to ask me which approach I prefer."
    )
    # --disallowedTools is the documented per-session disable switch
    extra = ["--disallowedTools", "AskUserQuestion"]
    s = Session(label="aq6", extra_args=extra, echo=False)
    s.send(prompt)
    if not wait_for_result(s, 1, 60):
        print("[!] turn did not complete in 60s")

    dump_path = os.path.join(OUT, "aq6_full.log")
    s.dump(dump_path)

    # Look for: system.init tools list, any AQ tool_use, result content
    tools_listed = []
    aq_attempts = []
    final_text = ""
    for (ts, lbl, line) in s.lines:
        if lbl != "stdout":
            continue
        try:
            j = json.loads(line)
        except Exception:
            continue
        if j.get("type") == "system" and j.get("subtype") == "init":
            tools_listed = j.get("tools", [])
        elif j.get("type") == "assistant":
            for c in j.get("message", {}).get("content", []):
                if c.get("type") == "tool_use" and c.get("name") == "AskUserQuestion":
                    aq_attempts.append({"ts": ts, "id": c.get("id")})
                elif c.get("type") == "text":
                    final_text = c.get("text", "")[:250]
        elif j.get("type") == "result":
            final_text = (j.get("result") or "")[:250]

    print(f"\n========== AQ6 FINDINGS ==========")
    print(f"  init tools includes AskUserQuestion? {'AskUserQuestion' in tools_listed}")
    print(f"  AskUserQuestion attempts in this turn: {len(aq_attempts)}")
    print(f"  final text (truncated): {final_text!r}")

    s.close(wait=3)
    print(f"\n[driver] dumped -> {dump_path}")
    return 0


if __name__ == "__main__":
    sys.exit(run())
