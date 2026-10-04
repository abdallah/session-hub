#!/usr/bin/env python3
"""Time `sessionhub plugin event` with a captured payload, in an isolated state dir.

Usage: SESSIONHUB_BIN=/path/to/sessionhub python3 task-4-latency.py <event.json>
SESSIONHUB_BIN defaults to `sessionhub` on PATH. State goes to a temporary directory that is
removed at the end.

warm: another process holds watcher.lock (a watcher is running), so the call
only appends. cold: the lock is free, so the call also spawns a watcher (which
exits at once: its socket path does not exist).
"""
import fcntl, os, shutil, statistics, subprocess, sys, tempfile, time

sessionhub = os.environ.get("SESSIONHUB_BIN") or shutil.which("sessionhub")
if not sessionhub:
    sys.exit("sessionhub not found: set SESSIONHUB_BIN or put sessionhub on PATH")
payload = open(sys.argv[1]).read()
tmp = tempfile.mkdtemp(prefix="sessionhub-latency-")
state = os.path.join(tmp, "state")
os.makedirs(state)
env = dict(os.environ,
           SESSIONHUB_STATE_DIR=state,
           SESSIONHUB_CONFIG=os.path.join(tmp, "nonexistent.toml"),
           HERDR_SOCKET_PATH=os.path.join(tmp, "no-herdr.sock"),
           HERDR_PLUGIN_EVENT="pane.agent_status_changed",
           HERDR_PLUGIN_EVENT_JSON=payload)


def run(n):
    out = []
    for _ in range(n):
        t = time.perf_counter()
        r = subprocess.run([sessionhub, "plugin", "event"], env=env, capture_output=True)
        out.append((time.perf_counter() - t) * 1000)
        assert r.returncode == 0, r
    return out


def show(name, xs):
    xs = sorted(xs)
    print(f"{name}: n={len(xs)} min={xs[0]:.1f}ms median={statistics.median(xs):.1f}ms "
          f"p95={xs[int(len(xs)*0.95)-1]:.1f}ms max={xs[-1]:.1f}ms")


try:
    print("sessionhub:", sessionhub)
    lock = open(os.path.join(state, "watcher.lock"), "a+")
    fcntl.flock(lock, fcntl.LOCK_EX)
    show("warm (watcher running)", run(50))
    fcntl.flock(lock, fcntl.LOCK_UN)
    lock.close()
    cold = []
    for _ in range(20):
        cold += run(1)
        time.sleep(0.05)  # let the spawned watcher exit and free the lock
    show("cold (spawns a watcher)", cold)
    with open(os.path.join(state, "queue.jsonl")) as f:
        print("queued lines:", sum(1 for _ in f))
finally:
    time.sleep(0.2)  # the last spawned watcher may still hold its log open
    shutil.rmtree(tmp, ignore_errors=True)
