"""
V3: 启用 --replay-user-messages 连发多条，数 replay 事件数和 result 事件数。
对 FIFO 匹配策略很关键：
- 如果 replay 数 == 我们发的 stdin 数 → CLI 每条都 ack，可以按 replay 对齐 uuid
- 如果 result 数 < stdin 数 → 确认 CLI 合并了多条，naozhi 必须处理多个 slot 共享一个 result
"""
import sys, os
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session, summarize

def run(label, extra=None):
    s = Session(label, extra_args=extra or [])
    uuids = []
    for i, word in enumerate(["APPLE", "BANANA", "CHERRY", "DATE", "ELDERBERRY"]):
        u = s.send(f"Reply with exactly one word: {word}. Nothing else.")
        uuids.append(u)
        s.wait_seconds(0.05)

    s.wait_for_results(5, timeout=120)
    summarize(label, s)

    user_events = s.user_events()
    # Classify user events into replays vs tool_results
    replays = [u for u in user_events if u.get("isReplay")]
    tool_results = []
    text_users = []
    for u in user_events:
        content = u.get("message", {}).get("content", [])
        if isinstance(content, list):
            if any(c.get("type") == "tool_result" for c in content):
                tool_results.append(u)
            elif any(c.get("type") == "text" for c in content):
                text_users.append(u)

    replay_uuids = [u.get("uuid") for u in replays]
    print(f"\n  [{label}] sent {len(uuids)} stdin msgs")
    print(f"  [{label}] uuid list: {uuids}")
    print(f"  [{label}] replays: {len(replays)} (uuids={replay_uuids})")
    print(f"  [{label}] text-user events (non-replay): {len(text_users)}")
    print(f"  [{label}] tool_result user events: {len(tool_results)}")
    print(f"  [{label}] result events: {s.result_count()}")
    for r in s.results():
        txt = (r.get('result') or '')[:60].replace('\n',' ')
        print(f"    result num_turns={r.get('num_turns')} text={txt!r}")

    s.close()
    s.dump(f"/tmp/{label.lower()}.ndjson")
    return {
        "stdin": len(uuids),
        "replays": len(replays),
        "text_users": len(text_users),
        "results": s.result_count(),
        "replay_uuids": replay_uuids,
        "sent_uuids": uuids,
    }

if __name__ == "__main__":
    print("========== V3a: with --replay-user-messages ==========")
    a = run("V3a", extra=["--replay-user-messages"])
    print("\n========== V3b: without --replay-user-messages ==========")
    b = run("V3b", extra=None)

    print("\n\n===== V3 verdict =====")
    print(f"V3a sent={a['stdin']} replays={a['replays']} results={a['results']}")
    print(f"V3b sent={b['stdin']} text_user_events={b['text_users']} results={b['results']}")

    # V3 PASS if --replay-user-messages produces replay for EVERY stdin message
    # (allowing 1 extra replay for command echoes)
    PASS_A = a["replays"] >= a["stdin"]
    uuid_match = set(a["replay_uuids"]) >= set(a["sent_uuids"])
    print(f"  V3a: replays cover all stdin? {PASS_A}   uuid round-tripped? {uuid_match}")
    sys.exit(0 if PASS_A and uuid_match else 1)
