"""
AQ7: what if CC mixes AskUserQuestion with a *real* tool call in the same
assistant message (e.g. Read + AskUserQuestion)?

This matters for naozhi's event filtering: we plan to suppress the auto-error
tool_result for AQ but NOT for other tools. Need to verify:
  - auto-errors only target AQ tool_use_id (not Read)
  - Read gets a proper tool_result with file contents
  - ordering is deterministic

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
        "Do two things in parallel:\n"
        "  (a) Use the Read tool to read the file /etc/hostname.\n"
        "  (b) Use the AskUserQuestion tool to ask me whether I want verbose "
        "or compact output.\n"
        "Issue both tool calls in the same response turn."
    )
    s = Session(label="aq7", echo=False)
    s.send(prompt)
    if not wait_for_result(s, 1, 60):
        print("[!] turn did not complete in 60s")

    dump_path = os.path.join(OUT, "aq7_full.log")
    s.dump(dump_path)

    events = []
    for (ts, lbl, line) in s.lines:
        if lbl != "stdout":
            continue
        try:
            j = json.loads(line)
        except Exception:
            continue
        if j.get("type") == "assistant":
            for c in j.get("message", {}).get("content", []):
                if c.get("type") == "tool_use":
                    events.append({"kind": "use", "ts": ts, "name": c.get("name"), "id": c.get("id")})
        elif j.get("type") == "user":
            for c in j.get("message", {}).get("content", []):
                if c.get("type") == "tool_result":
                    content = c.get("content", "")
                    if isinstance(content, list):
                        content = json.dumps(content)[:60]
                    elif isinstance(content, str):
                        content = content[:60]
                    events.append({
                        "kind": "result",
                        "ts": ts,
                        "tool_use_id": c.get("tool_use_id"),
                        "is_error": c.get("is_error", False),
                        "content": content,
                    })

    print("\n========== AQ7 EVENT ORDER ==========")
    for e in events:
        if e["kind"] == "use":
            print(f"  t+{e['ts']:5.2f}s USE    {e['name']:20s} id={e['id'][-12:]}")
        else:
            err = " ERR" if e["is_error"] else "    "
            print(f"  t+{e['ts']:5.2f}s RESULT{err}                 tuid={e['tool_use_id'][-12:]}  content={e['content']!r}")

    # Validate: each tool_use has a matching tool_result.
    # AQ uses → is_error:true auto-errors; others → normal tool_results.
    uses = {e["id"]: e["name"] for e in events if e["kind"] == "use"}
    results_by_tuid = {e["tool_use_id"]: e for e in events if e["kind"] == "result"}

    print(f"\n  tool_uses:   {len(uses)}")
    print(f"  tool_results:{len(results_by_tuid)}")
    issues = []
    for use_id, name in uses.items():
        r = results_by_tuid.get(use_id)
        if not r:
            issues.append(f"{name}({use_id[-8:]}) has no matching result")
            continue
        if name == "AskUserQuestion" and not r["is_error"]:
            issues.append(f"{name}({use_id[-8:]}) result not marked is_error — unexpected")
        if name != "AskUserQuestion" and r["is_error"]:
            issues.append(f"{name}({use_id[-8:]}) got is_error — affects our filtering assumption")

    if issues:
        print("  [!] issues:")
        for i in issues:
            print(f"    - {i}")
    else:
        print("  verdict: AQ auto-error and real tool_results are cleanly separable by tool_use_id")

    s.close(wait=3)
    print(f"\n[driver] dumped -> {dump_path}")
    return 0


if __name__ == "__main__":
    sys.exit(run())
