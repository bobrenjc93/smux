#!/usr/bin/env python3
"""Capture golden traces of tmux -CC control mode.

Runs tmux in control mode under a pty, sends scripted commands, and records
the raw byte stream tmux emits. Used to pin down the exact wire format smux
must reproduce.
"""
import os
import pty
import select
import subprocess
import sys
import time

SOCK = "smux-golden-trace"


def run_trace(name, script, initial_cmd, kill_first=True):
    """script: list of (delay_seconds, bytes_to_send)."""
    if kill_first:
        os.system(f"tmux -L {SOCK} kill-server 2>/dev/null")
        time.sleep(0.2)
    master, slave = pty.openpty()
    p = subprocess.Popen(
        ["tmux", "-CC", "-f", "/dev/null", "-L", SOCK] + initial_cmd,
        stdin=slave, stdout=slave, stderr=slave, close_fds=True,
    )
    os.close(slave)
    out = bytearray()
    deadline = time.time() + 15
    script = list(script)
    next_send = time.time() + script[0][0] if script else None
    while time.time() < deadline:
        timeout = 0.05
        r, _, _ = select.select([master], [], [], timeout)
        if r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            out.extend(data)
        if next_send and time.time() >= next_send:
            delay, payload = script.pop(0)
            os.write(master, payload)
            next_send = time.time() + script[0][0] if script else None
        if p.poll() is not None and not r:
            break
    try:
        os.close(master)
    except OSError:
        pass
    p.wait()
    os.system(f"tmux -L {SOCK} kill-server 2>/dev/null")
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        "traces", f"{name}.raw")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(bytes(out))
    print(f"=== {name}: {len(out)} bytes -> {path}")
    sys.stdout.write(bytes(out).decode("utf-8", "backslashreplace")
                     .replace("\x1b", "<ESC>").replace("\r\n", "\n"))
    print("\n=== end", name)


if __name__ == "__main__":
    which = sys.argv[1] if len(sys.argv) > 1 else "all"

    if which in ("all", "new"):
        # Fresh session: startup handshake, then typical iTerm2-ish queries.
        run_trace("new_session", [
            (1.0, b'display-message -p "#{version}"\n'),
            (0.3, b'list-sessions -F "#{session_id} #{session_name}"\n'),
            (0.3, b'list-windows -F "#{window_id} #{window_layout} #{window_flags}" -t "$0"\n'),
            (0.3, b'new-window -PF "#{window_id}"\n'),
            (0.5, b'send-keys -t %0 -H 65 63 68 6f 20 68 69 0d\n'),
            (0.8, b'capture-pane -p -t %0\n'),
            (0.3, b'bogus-command-xyz\n'),
            (0.3, b'kill-window -t @1\n'),
            (0.3, b'detach-client\n'),
        ], ["new-session"])

    if which in ("all", "attach"):
        # Create detached session first, then attach with -CC.
        os.system(f"tmux -L {SOCK} kill-server 2>/dev/null")
        time.sleep(0.2)
        os.system(f"tmux -f /dev/null -L {SOCK} new-session -d -s main")
        os.system(f"tmux -f /dev/null -L {SOCK} new-window -t main")
        time.sleep(0.3)
        run_trace("attach", [
            (1.0, b'list-windows -F "#{window_id} #{window_layout} #{window_flags}"\n'),
            (0.3, b'detach-client\n'),
        ], ["attach-session", "-t", "main"], kill_first=False)

    if which in ("all", "output"):
        # Escaping of control bytes / UTF-8 in %output.
        run_trace("output_escaping", [
            (1.0, b'send-keys -t %0 -H 70 72 69 6e 74 66 20 27 61 5c 74 62 5c 6e 5c 33 33 33 5c 30 30 37 20 c3 a9 e4 b8 ad 5c 5c 78 27 0d\n'),
            (1.0, b'kill-session -t $0\n'),
        ], ["new-session"])
