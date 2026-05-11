"""
AQ1 + AQ2: Can we trigger AskUserQuestion from CC, and what does the event look like?

Strategy:
  - Start a single claude CLI in stream-json mode (re-use passthrough harness primitives).
  - Send a prompt that's ambiguous in a way that *should* make CC want to ask a question.
  - Dump every stdout NDJSON line to disk.
  - Grep for tool_use blocks with name="AskUserQuestion" and print the raw input schema.

We intentionally do NOT answer here — we want to observe pre-answer state too
(for AQ3: does CC block? does result arrive?).

Budget: 1 CLI spawn, 1 user prompt, up to 60s wait.
"""
from __future__ import annotations
import json
import os
import sys
import time

# Let us import harness.py from the passthrough dir
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "passthrough"))
from harness import Session, umsg  # noqa: E402

OUT = os.path.join(HERE, "out")
os.makedirs(OUT, exist_ok=True)


def run() -> int:
    # Prompt engineered to nudge CC into AskUserQuestion:
    # - Multiple valid paths
    # - Explicit "ask me" hint
    # - Mention the tool by expected name to bias tool selection
    prompt = (
        "I want to add error handling to a function. I'm not sure whether to "
        "use panic, return an error, or log-and-continue — the tradeoffs "
        "depend on my use case. Before doing anything, use the AskUserQuestion "
        "tool to ask me which approach I prefer. Do not guess. Do not write code yet."
    )
    s = Session(label="aq1", echo=True)
    u = s.send(prompt)
    print(f"\n[driver] sent user msg uuid={u}\n")

    # Wait up to 45s for either (a) AskUserQuestion tool_use, or (b) turn end result.
    deadline = time.time() + 45
    saw_ask = False
    while time.time() < deadline:
        time.sleep(0.3)
        # Parse what we have so far
        for (_, lbl, line) in s.lines:
            if lbl != "stdout":
                continue
            try:
                j = json.loads(line)
            except Exception:
                continue
            if j.get("type") == "assistant":
                for c in j.get("message", {}).get("content", []):
                    if c.get("type") == "tool_use" and c.get("name") == "AskUserQuestion":
                        saw_ask = True
        if saw_ask:
            break
        if s.result_count() >= 1:
            break

    # Wait a short extra window to see if CC sends anything else (result event,
    # more thinking, etc) after it emits AskUserQuestion.
    time.sleep(3)

    dump_path = os.path.join(OUT, "aq1_aq2_stream.log")
    s.dump(dump_path)
    print(f"\n[driver] dumped {len(s.lines)} lines -> {dump_path}")

    # Extract AskUserQuestion payloads verbatim
    ask_events = []
    last_timestamp = None
    for (ts, lbl, line) in s.lines:
        if lbl != "stdout":
            continue
        try:
            j = json.loads(line)
        except Exception:
            continue
        last_timestamp = ts
        if j.get("type") == "assistant":
            for c in j.get("message", {}).get("content", []):
                if c.get("type") == "tool_use" and c.get("name") == "AskUserQuestion":
                    ask_events.append((ts, j, c))

    results = s.results()

    print("\n========== AQ1 / AQ2 FINDINGS ==========")
    print(f"  AskUserQuestion tool_use seen: {len(ask_events)}")
    print(f"  result events seen: {len(results)}")
    if last_timestamp is not None:
        print(f"  last stdout line at: {last_timestamp:.2f}s")

    if ask_events:
        ts, _, block = ask_events[0]
        print(f"\n  first AskUserQuestion at t+{ts:.2f}s")
        print(f"  tool_use id: {block.get('id')!r}")
        print(f"  tool_use name: {block.get('name')!r}")
        print("  input (verbatim):")
        print("    " + json.dumps(block.get("input", {}), ensure_ascii=False, indent=2).replace("\n", "\n    "))
    else:
        print("\n  [!] no AskUserQuestion triggered — prompt needs adjustment")

    # For AQ3 pre-answer: did result arrive? If YES, CC might NOT actually
    # block waiting for tool_result (worth flagging).
    if ask_events and results:
        ask_ts = ask_events[0][0]
        result_ts = None
        for (ts, lbl, line) in s.lines:
            if lbl != "stdout":
                continue
            try:
                j = json.loads(line)
            except Exception:
                continue
            if j.get("type") == "result":
                result_ts = ts
                break
        print(f"\n  [AQ3 preview] result arrived {'BEFORE' if result_ts and result_ts < ask_ts else 'AFTER'} AskUserQuestion (ask=+{ask_ts:.2f}s, result=+{(result_ts or 0):.2f}s)")
    elif ask_events and not results:
        print("\n  [AQ3 preview] AskUserQuestion seen, NO result yet — consistent with 'CC blocks waiting for tool_result'")

    s.close(wait=3)
    return 0 if ask_events else 1


if __name__ == "__main__":
    sys.exit(run())
