"""
V6: 验证自生成 uuid 在 result 里的回传情况。

实际上 CC 的 result 事件不会带 user uuid。我们要确认的是：
- replay 事件会原样带回我们发的 uuid  (这是唯一稳定的匹配点)
- 在没有启用 --replay-user-messages 的情况下，还有无其他方式回传 uuid
"""
import sys, os, json
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session

def test_with_replay():
    s = Session("V6replay", extra_args=["--replay-user-messages"])
    sent = {}
    for w in ["ONE", "TWO", "THREE"]:
        u = s.send(f"Reply only: {w}.")
        sent[u] = w
        s.wait_seconds(0.1)
    s.wait_for_results(3, timeout=60)

    replays = [u for u in s.user_events() if u.get("isReplay")]
    replay_uuids = {u.get("uuid") for u in replays}
    print(f"  sent uuids: {list(sent.keys())}")
    print(f"  replay uuids: {list(replay_uuids)}")
    print(f"  all sent uuids present in replays? {set(sent.keys()) <= replay_uuids}")

    results = s.results()
    result_session_ids = {r.get("session_id") for r in results}
    print(f"  result session_ids: {result_session_ids}")

    # Are there any uuid references in the result events?
    for r in results:
        keys_with_uuid = [k for k in r if "uuid" in k.lower()]
        print(f"  result keys with 'uuid': {keys_with_uuid}  values: {[r[k] for k in keys_with_uuid]}")

    s.close()
    s.dump("/tmp/v6_replay.ndjson")
    return set(sent.keys()) <= replay_uuids

def test_without_replay():
    s = Session("V6plain")
    u = s.send("Reply only: HELLO.")
    s.wait_for_results(1, timeout=30)
    results = s.results()
    r = results[0] if results else {}
    # Serialize and check if our uuid appears anywhere in the result payload
    blob = json.dumps(r)
    print(f"  sent uuid: {u}")
    print(f"  uuid in result payload? {u in blob}")
    s.close()
    s.dump("/tmp/v6_plain.ndjson")
    return u in blob

if __name__ == "__main__":
    print("========== V6a: uuid tracking with --replay-user-messages ==========")
    replay_ok = test_with_replay()
    print("\n========== V6b: uuid tracking without replay flag ==========")
    plain_ok = test_without_replay()

    print(f"\nV6a (replay uuid round-trip): {'PASS' if replay_ok else 'FAIL'}")
    print(f"V6b (plain uuid round-trip): {'PASS' if plain_ok else 'FAIL — need replay flag'}")
    sys.exit(0 if replay_ok else 1)
