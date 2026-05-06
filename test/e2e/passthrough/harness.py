"""
Test harness for passthrough-mode CLI behavior verification.
Reusable primitives for V1-V9 scripts.
"""
from __future__ import annotations
import json
import subprocess
import threading
import time
import uuid as uuidmod
from typing import Any, Optional, List, Tuple

CLAUDE_BIN = "claude"

def start_claude(extra_args: Optional[List[str]] = None) -> subprocess.Popen:
    args = [
        CLAUDE_BIN, "-p",
        "--output-format", "stream-json",
        "--input-format", "stream-json",
        "--verbose",
        "--setting-sources", "",
        "--dangerously-skip-permissions",
    ]
    if extra_args:
        args += extra_args
    return subprocess.Popen(
        args,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        bufsize=0,
    )

def umsg(text: str, *, priority: Optional[str] = None, uuid: Optional[str] = None) -> str:
    msg: dict = {
        "type": "user",
        "message": {
            "role": "user",
            "content": [{"type": "text", "text": text}],
        },
    }
    if priority is not None:
        msg["priority"] = priority
    if uuid is not None:
        msg["uuid"] = uuid
    return json.dumps(msg) + "\n"

def ctrl_interrupt(request_id: str = "test-1") -> str:
    return json.dumps({
        "type": "control_request",
        "request_id": request_id,
        "request": {"subtype": "interrupt"},
    }) + "\n"

class Session:
    def __init__(self, label: str, extra_args: Optional[List[str]] = None, echo: bool = True):
        self.label = label
        self.echo = echo
        self.t0 = time.time()
        self.p = start_claude(extra_args)
        self.lines: List[Tuple[float, str, str]] = []
        self._stop = threading.Event()
        threading.Thread(target=self._reader, args=(self.p.stdout, "stdout"), daemon=True).start()
        threading.Thread(target=self._reader, args=(self.p.stderr, "stderr"), daemon=True).start()

    def _reader(self, stream, lbl: str) -> None:
        for raw in stream:
            if self._stop.is_set():
                return
            try:
                s = raw.decode("utf-8", errors="replace").rstrip()
            except Exception:
                s = str(raw)
            ts = time.time() - self.t0
            self.lines.append((ts, lbl, s))
            if self.echo:
                short = s[:200].replace("\n", " ")
                print(f"[{self.label} {ts:6.2f}s] {lbl}: {short}")

    def send_raw(self, raw: str) -> None:
        if self.echo:
            t = time.time() - self.t0
            short = raw.strip()[:120]
            print(f"[{self.label} {t:6.2f}s] -> RAW: {short}")
        self.p.stdin.write(raw.encode())
        self.p.stdin.flush()

    def send(self, text: str, *, priority: Optional[str] = None, uuid: Optional[str] = None) -> str:
        u = uuid or str(uuidmod.uuid4())
        self.send_raw(umsg(text, priority=priority, uuid=u))
        return u

    def interrupt(self, req_id: str = "test-1") -> None:
        self.send_raw(ctrl_interrupt(req_id))

    def wait_for_results(self, n: int, timeout: float = 60) -> bool:
        deadline = time.time() + timeout
        while time.time() < deadline:
            time.sleep(0.2)
            if self.result_count() >= n:
                return True
        return False

    def wait_seconds(self, secs: float) -> None:
        time.sleep(secs)

    def result_count(self) -> int:
        return sum(1 for (_, lbl, s) in self.lines if lbl == "stdout" and '"type":"result"' in s)

    def results(self) -> list:
        out = []
        for (_, lbl, s) in self.lines:
            if lbl != "stdout": continue
            try:
                j = json.loads(s)
            except Exception:
                continue
            if j.get("type") == "result":
                out.append(j)
        return out

    def thinking_texts(self) -> list:
        out = []
        for (_, lbl, s) in self.lines:
            if lbl != "stdout": continue
            try:
                j = json.loads(s)
            except Exception:
                continue
            if j.get("type") == "assistant":
                for c in j.get("message", {}).get("content", []):
                    if c.get("type") == "thinking":
                        out.append(c.get("thinking", ""))
        return out

    def user_events(self) -> list:
        """All user events on stdout (replays + tool_results)."""
        out = []
        for (_, lbl, s) in self.lines:
            if lbl != "stdout": continue
            try:
                j = json.loads(s)
            except Exception:
                continue
            if j.get("type") == "user":
                out.append(j)
        return out

    def close(self, wait: float = 10) -> None:
        self._stop.set()
        try:
            self.p.stdin.close()
        except Exception:
            pass
        try:
            self.p.wait(timeout=wait)
        except Exception:
            try: self.p.kill()
            except Exception: pass

    def kill(self) -> None:
        try:
            self.p.kill()
        except Exception:
            pass

    def dump(self, path: str) -> None:
        with open(path, "w") as f:
            for ts, lbl, s in self.lines:
                f.write(f"{ts:7.3f}\t{lbl}\t{s}\n")


def summarize(label: str, s: Session) -> None:
    print(f"\n========== {label} SUMMARY ==========")
    print(f"  result count: {s.result_count()}")
    rs = s.results()
    for i, r in enumerate(rs):
        nt = r.get("num_turns")
        txt = (r.get("result") or "")[:150].replace("\n", " ")
        sub = r.get("subtype")
        stop = r.get("stop_reason")
        print(f"  [{i}] subtype={sub} stop={stop} num_turns={nt} result={txt!r}")
    print(f"  user events: {len(s.user_events())}")
