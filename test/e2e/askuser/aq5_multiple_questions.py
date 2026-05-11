"""
AQ5: what happens if CC wants to AskUserQuestion *twice* in a row?

Hypothesis: same auto-error pattern repeats — each AQ tool_use gets an
immediate is_error:true tool_result and CC emits a final summary text.

Also: what if CC calls AskUserQuestion mid-stream alongside other tool_use
blocks in the same assistant message? Does each get its own auto-error?

Budget: 1 CLI spawn, 1 carefully crafted prompt.
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


def run() -> int:
    # This prompt pushes for multiple independent questions.
    prompt = (
        "I'm about to start a new project. Before writing any code, "
        "use the AskUserQuestion tool AT LEAST TWICE in separate calls "
        "to gather my preferences:\n"
        "  1. First ask about the programming language.\n"
        "  2. Then ask about the build system.\n"
        "  3. Then ask about testing framework.\n"
        "Make each a separate AskUserQuestion tool call. Do not combine them."
    )
    s = Session(label="aq5", echo=False)
    s.send(prompt)
    if not wait_for_result(s, 1, 60):
        print("[!] turn did not complete in 60s")

    dump_path = os.path.join(OUT, "aq5_full.log")
    s.dump(dump_path)

    # Count AskUserQuestion tool_uses + their matching auto-error tool_results
    aq_uses = []
    auto_errors = []
    for (ts, lbl, line) in s.lines:
        if lbl != "stdout":
            continue
        try:
            j = json.loads(line)
        except Exception:
            continue
        if j.get("type") == "assistant":
            for c in j.get("message", {}).get("content", []):
                if c.get("type") == "tool_use" and c.get("name") == "AskUserQuestion":
                    aq_uses.append({"ts": ts, "id": c.get("id"), "input": c.get("input")})
        elif j.get("type") == "user":
            for c in j.get("message", {}).get("content", []):
                if c.get("type") == "tool_result" and c.get("is_error") and c.get("content") == "Answer questions?":
                    auto_errors.append({"ts": ts, "tool_use_id": c.get("tool_use_id")})

    print(f"\n========== AQ5 FINDINGS ==========")
    print(f"  AskUserQuestion tool_uses: {len(aq_uses)}")
    for i, a in enumerate(aq_uses):
        qs = a["input"].get("questions", [])
        print(f"    [{i}] t+{a['ts']:5.2f}s id={a['id'][-12:]} questions={[q.get('header') for q in qs]}")
    print(f"  auto-error tool_results: {len(auto_errors)}")
    for i, e in enumerate(auto_errors):
        print(f"    [{i}] t+{e['ts']:5.2f}s tool_use_id={e['tool_use_id'][-12:]}")

    # Verify 1:1 pairing
    ids_used = {a["id"] for a in aq_uses}
    ids_erred = {e["tool_use_id"] for e in auto_errors}
    unmatched_uses = ids_used - ids_erred
    unmatched_errs = ids_erred - ids_used
    print(f"  unmatched AQ uses (no matching error): {unmatched_uses}")
    print(f"  unmatched auto-errors (no matching AQ): {unmatched_errs}")

    if aq_uses and len(aq_uses) == len(auto_errors) and not unmatched_uses and not unmatched_errs:
        print("  verdict: each AQ gets its own 1:1 auto-error tool_result")
    elif aq_uses:
        print("  verdict: MISMATCH — pairing broken")
    else:
        print("  verdict: no AQ triggered (prompt may have failed to induce it)")

    s.close(wait=3)
    print(f"\n[driver] dumped -> {dump_path}")
    return 0


if __name__ == "__main__":
    sys.exit(run())
