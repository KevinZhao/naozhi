"""
AQ3 full lifecycle: what does CC do after the auto-injected tool_result error?

AQ1/AQ2 revealed that `claude -p` mode auto-injects:
  user { tool_result, is_error:true, content:"Answer questions?", tool_use_id: <ask> }
~3ms after the AskUserQuestion tool_use. This means CC does NOT block in headless mode.

Question now: does CC eventually emit a `result` event? Does it try to proceed?
Does it loop trying to ask again? Run 90s, dump everything, no answer attempt.

Budget: 1 CLI spawn.
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


def run() -> int:
    prompt = (
        "I want to add error handling to a function. I'm not sure whether to "
        "use panic, return an error, or log-and-continue. Before doing anything, "
        "use the AskUserQuestion tool to ask me which approach I prefer. Do not "
        "guess. Do not write code yet."
    )
    s = Session(label="aq3", echo=False)
    u = s.send(prompt)
    print(f"[driver] sent user msg uuid={u}")

    # Just wait 90s observing everything.
    deadline = time.time() + 90
    last_count = 0
    while time.time() < deadline:
        time.sleep(2)
        n = len(s.lines)
        if n != last_count:
            last_count = n
            rc = s.result_count()
            print(f"  [watch t+{time.time()-s.t0:5.1f}s] lines={n} results={rc}")
            if rc >= 1:
                # Give it 3 more seconds after first result
                time.sleep(3)
                break

    dump_path = os.path.join(OUT, "aq3_full.log")
    s.dump(dump_path)

    print("\n========== AQ3 TIMELINE (stdout only) ==========")
    for (ts, lbl, line) in s.lines:
        if lbl != "stdout":
            continue
        try:
            j = json.loads(line)
        except Exception:
            continue
        t = j.get("type")
        st = j.get("subtype", "")
        summary = ""
        if t == "assistant":
            for c in j.get("message", {}).get("content", []):
                ct = c.get("type")
                if ct == "thinking":
                    summary += f"[thinking:{c.get('thinking','')[:80]!r}] "
                elif ct == "tool_use":
                    summary += f"[tool_use:{c.get('name')} id={c.get('id','')[-12:]}] "
                elif ct == "text":
                    summary += f"[text:{c.get('text','')[:80]!r}] "
        elif t == "user":
            for c in j.get("message", {}).get("content", []):
                ct = c.get("type")
                if ct == "tool_result":
                    err = " (is_error)" if c.get("is_error") else ""
                    content = c.get("content", "")
                    if isinstance(content, str):
                        summary += f"[tool_result{err} tuid={c.get('tool_use_id','')[-12:]}: {content[:60]!r}] "
                    else:
                        summary += f"[tool_result{err} tuid={c.get('tool_use_id','')[-12:]}] "
        elif t == "result":
            summary = f"subtype={st} stop_reason={j.get('stop_reason')} is_error={j.get('is_error')} num_turns={j.get('num_turns')}"
            rtxt = (j.get("result") or "")[:120]
            if rtxt:
                summary += f" result={rtxt!r}"
        elif t == "system":
            summary = f"subtype={st}"
        print(f"  t+{ts:5.2f}s  {t:12s} {summary}")

    results = s.results()
    print(f"\n[driver] results: {len(results)}, dumped -> {dump_path}")
    s.close(wait=3)
    return 0


if __name__ == "__main__":
    sys.exit(run())
