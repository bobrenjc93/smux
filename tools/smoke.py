#!/usr/bin/env python3
"""Manual smoke test: speak the smux client socket protocol directly,
mimicking the command sequence iTerm2 sends, and print the conversation."""
import json
import os
import socket
import subprocess
import sys
import time

TMPDIR = "/tmp/smux-smoke"
SOCK = TMPDIR + "/default"
SMUX = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "smux")


def connect(cmd):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.connect(SOCK)
    s.sendall((json.dumps({"cmd": cmd}) + "\n").encode())
    s.settimeout(0.5)
    return s


def drain(s, secs=0.6):
    out = bytearray()
    end = time.time() + secs
    while time.time() < end:
        try:
            d = s.recv(65536)
            if not d:
                break
            out.extend(d)
        except socket.timeout:
            pass
    return bytes(out)


def show(label, data):
    print(f"--- {label}")
    print(data.decode("utf-8", "backslashreplace").replace("\x1b", "<ESC>")
          .replace("\r\n", "\n"), end="")
    print("--- end")


def main():
    os.system(f"SMUX_TMPDIR={TMPDIR} {SMUX} kill-server 2>/dev/null; rm -rf {TMPDIR}")
    env = dict(os.environ, SMUX_TMPDIR=TMPDIR)
    # Start server via a throwaway -CC client on a pty-free pipe: instead
    # spawn the server directly.
    subprocess.Popen([SMUX, "--server"], env=env, start_new_session=True)
    for _ in range(100):
        if os.path.exists(SOCK):
            break
        time.sleep(0.05)

    c = connect("new")
    show("handshake", drain(c, 1.0))

    script = [
        b'\x03',  # anti-RCE ^C, merges with next line
        b'phony-command\r',
        b'refresh-client -fpause-after=0,wait-exit\r',
        b'show-window-options -g aggressive-resize\r',
        b'show-option -g -v status\r',
        b'list-sessions -F "\t"\r',
        b'show-options -v -s default-terminal\r',
        b'list-keys\r',
        b'copy-mode -q\r',
        b'display-message -p "#{version}"\r',
        b'show-window-options pane-border-format\r',
        b'list-windows -F "#{socket_path}"\r',
        b'list-windows -F "#{pid}"\r',
        b'show-options -g message-style\r',
        b'refresh-client -fpause-after=120\r',
        b'display-message -p "#{pid}"\r',
        b'show-options -v -g set-titles\r',
        b'show -v -q -t $0 @iterm2_size\r',
        b'show -v -q -t $0 @iterm2_id; refresh-client -C 90,30; show -v -q -t $0 @hidden; show -v -q -t $0 @buried_indexes; show -v -q -t $0 @affinities; show -v -q -t $0 @per_window_settings; show -v -q -t $0 @per_tab_settings; show -v -q -t $0 @origins; show -v -q -t $0 @hotkeys; show -v -q -t $0 @tab_colors; list-sessions -F "#{session_id} #{session_name}"; list-windows -F "#{session_name}\t#{window_id}\t#{window_name}\t#{window_width}\t#{window_height}\t#{window_layout}\t#{window_flags}\t#{?window_active,1,0}\t#{window_visible_layout}\t#{pane-border-status}"\r',
        b'set -t $0 @iterm2_id "test-guid-1234"\r',
        b'capture-pane -peqJN -t "%0" -S -2000; capture-pane -peqJN -a -t "%0" -S -2000; list-panes -t "%0" -F "pane_id=#{pane_id}\talternate_on=#{alternate_on}\tcursor_x=#{cursor_x}\tcursor_y=#{cursor_y}"; capture-pane -p -P -C -t "%0"; refresh-client -A \'%0:continue\'\r',
        b'send -lt %0 echo\r',
        b'send -t %0 0x20\r',
        b'send -lt %0 hello-smux\r',
        b'send -H -t %0 0d\r',
    ]
    for cmd in script:
        c.sendall(cmd)
        time.sleep(0.15)
    show("startup battery", drain(c, 1.5))

    c.sendall(b'refresh-client -C 100,40; new-window -PF \'#{window_id}\'\r')
    time.sleep(0.3)
    c.sendall(b'display -p -F "#{session_name}\t#{window_id}\t#{window_name}" -t @1\r')
    show("cmd+t", drain(c, 1.0))

    # Abrupt disconnect (simulates SSH drop).
    c.close()
    time.sleep(0.3)

    # Reattach.
    c2 = connect("attach")
    show("reattach", drain(c2, 1.0))
    c2.sendall(b'capture-pane -peqJN -t "%0" -S -2000\r')
    show("capture after reattach", drain(c2, 1.0))
    c2.sendall(b'kill-window -t @1\r')
    show("kill @1", drain(c2, 0.8))
    c2.sendall(b'detach\r')
    show("detach", drain(c2, 0.8))

    subprocess.run([SMUX, "kill-server"], env=env)


if __name__ == "__main__":
    main()
