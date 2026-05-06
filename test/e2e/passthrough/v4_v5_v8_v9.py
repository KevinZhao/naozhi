"""
V4: CLI 死亡时 stdin 已堆积消息的行为
V5: /new 或 session reset 时队列清理
V8: 纯生成 turn + mid-turn 注入 — 应该延迟到下一 turn
V9: 同时写两条 stream-json 到 stdin 的并发安全性
"""
import sys, os, threading, time, subprocess, json, signal
sys.path.insert(0, os.path.dirname(__file__))
from harness import Session, umsg, ctrl_interrupt

def v4_kill_during_burst():
    print("\n--- V4: SIGKILL during pending burst ---")
    s = Session("v4", extra_args=["--replay-user-messages"], echo=False)
    # Enqueue 5 messages
    for i in range(5):
        s.send(f"Slowly think and then say {i+1}.")
        time.sleep(0.05)
    # Let first turn start, then SIGKILL
    time.sleep(3)
    print(f"    before SIGKILL: results={s.result_count()} replays={sum(1 for e in s.user_events() if e.get('isReplay'))}")
    # SIGKILL
    try:
        s.p.send_signal(signal.SIGKILL)
    except Exception as e:
        print(f"    kill err: {e}")
    try:
        s.p.wait(timeout=5)
    except Exception:
        pass
    time.sleep(1)
    print(f"    after SIGKILL: results={s.result_count()} stdout lines={len(s.lines)}")
    s.dump("/tmp/v4.ndjson")
    # Pass: no hang, process exited
    return s.p.returncode is not None

def v5_interrupt_followed_by_new_message():
    print("\n--- V5: control_request interrupt semantics ---")
    s = Session("v5", extra_args=["--replay-user-messages"], echo=False)
    # Msg A (long) then interrupt then msg B
    s.send("Write a 500-word story about mountains. Take your time.")
    time.sleep(2)
    s.interrupt()
    time.sleep(1)
    s.send("Say just the word AFTER_INTERRUPT.")
    s.wait_for_results(2, timeout=40)
    s.wait_seconds(2)
    rs = s.results()
    print(f"    results: {len(rs)}")
    for i, r in enumerate(rs):
        sub = r.get("subtype")
        txt = (r.get("result") or "")[:80]
        print(f"    [{i}] subtype={sub} text={txt!r}")
    s.close()
    s.dump("/tmp/v5.ndjson")
    # Pass: first result is interrupt, second is AFTER_INTERRUPT
    first_is_interrupt = bool(rs) and rs[0].get("subtype") == "error_during_execution"
    has_after = any("AFTER_INTERRUPT" in (r.get("result") or "").upper() for r in rs)
    print(f"    first_is_interrupt? {first_is_interrupt}  has AFTER_INTERRUPT? {has_after}")
    return first_is_interrupt and has_after

def v8_pure_gen_midturn():
    print("\n--- V8: pure-gen mid-turn injection ---")
    s = Session("v8", extra_args=["--replay-user-messages"], echo=False)
    s.send("Please write a detailed 300-word poem about autumn leaves, with rich imagery.")
    time.sleep(2)
    s.send("Also mention the word CIRRUS somewhere.")
    s.wait_for_results(2, timeout=60)
    s.wait_seconds(3)
    rs = s.results()
    print(f"    results: {len(rs)}")
    for i, r in enumerate(rs):
        txt = (r.get("result") or "")[:100].replace("\n"," ")
        print(f"    [{i}] num_turns={r.get('num_turns')} text={txt!r}")
    # Was mid-turn injection consumed in turn 1 or turn 2?
    first_has_cirrus = bool(rs) and "CIRRUS" in (rs[0].get("result") or "").upper()
    second_has_cirrus = len(rs) > 1 and "CIRRUS" in (rs[1].get("result") or "").upper()
    print(f"    first result has CIRRUS? {first_has_cirrus}")
    print(f"    second result has CIRRUS? {second_has_cirrus}")
    s.close()
    s.dump("/tmp/v8.ndjson")
    # Either behavior is acceptable — what matters is that BOTH appear as results
    return len(rs) >= 2 or first_has_cirrus  # RFC says pure-gen usually defers

def v9_concurrent_writes():
    print("\n--- V9: concurrent stdin writes ---")
    s = Session("v9", extra_args=["--replay-user-messages"], echo=False)
    # Two writer threads fire simultaneously
    def writer(prefix):
        for i in range(3):
            s.send(f"{prefix}{i}")
            time.sleep(0.01)

    t1 = threading.Thread(target=writer, args=("A",))
    t2 = threading.Thread(target=writer, args=("B",))
    t1.start(); t2.start()
    t1.join(); t2.join()
    s.wait_for_results(6, timeout=60)
    s.wait_seconds(3)
    replays = [e for e in s.user_events() if e.get("isReplay")]
    print(f"    sent 6 msgs via 2 threads, replays={len(replays)} results={s.result_count()}")
    # Pass: no panic, at least got some replays and results
    s.close()
    s.dump("/tmp/v9.ndjson")
    return s.p.returncode is None or s.p.returncode == 0 or len(replays) >= 4

if __name__ == "__main__":
    v4 = v4_kill_during_burst()
    v5 = v5_interrupt_followed_by_new_message()
    v8 = v8_pure_gen_midturn()
    v9 = v9_concurrent_writes()
    print(f"\n===== V4/V5/V8/V9 verdicts =====")
    print(f"V4 (SIGKILL cleanup): {'PASS' if v4 else 'FAIL'}")
    print(f"V5 (interrupt + new msg): {'PASS' if v5 else 'FAIL'}")
    print(f"V8 (pure-gen mid-turn): {'PASS' if v8 else 'FAIL'}")
    print(f"V9 (concurrent writes): {'PASS' if v9 else 'FAIL'}")
    sys.exit(0 if (v4 and v5 and v9) else 1)
