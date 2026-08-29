# smux

A minimal, reliability-first replacement for the subset of tmux that
iTerm2's tmux integration needs.

smux speaks the tmux control-mode wire protocol (`tmux -CC`), so iTerm2
treats it exactly like tmux: native tabs, Cmd+T, native window resizing,
and sessions that survive SSH disconnects. It is **not** a general tmux
replacement — it deliberately implements nothing beyond what iTerm2's
integration exercises.

## Usage

```sh
smux -CC        # create a new session (starts the server if needed)
smux -CC a      # attach to the most recent session
smux ls         # list sessions
smux kill-server
```

Typical iTerm2 workflow on a remote host:

```sh
ssh myhost -t 'smux -CC a || smux -CC'
```

Run that inside iTerm2 (or any tmux-control-mode client, e.g. dispatcher)
and it opens the session as native windows/tabs. If the SSH connection
drops, everything keeps running server-side; SSH back in, run `smux -CC a`,
and your tabs are restored, including scrollback, with all shells and
processes intact.

**Don't run `smux -CC` inside an existing tmux/smux session** (e.g. inside
a pane of a `tmux -CC` integration you're replacing). The outer multiplexer
swallows the control stream, so your terminal app never sees it. smux
refuses to start in that situation, like tmux does; run it from a plain
tab with a fresh SSH connection instead.

## Scope

Supported, because iTerm2's control-mode integration needs it:

- Sessions, windows (one pane each — iTerm2 tabs), attach/detach.
- Creating tabs (Cmd+T → `new-window`), closing tabs (`kill-window`),
  renaming, selecting, swapping (tab drag).
- Keyboard input in all three encodings iTerm2 uses (`send -lt`, hex code
  points, `send -H` raw bytes) plus tmux key names (`C-x`, `Up`, `F5`, …).
- Per-window sizing (`refresh-client -C`, `resize-window`) with
  `%layout-change` notifications.
- Content restore on reattach via `capture-pane` (a small server-side
  terminal-screen model tracks scrollback, SGR attributes, the alternate
  screen, cursor state).
- The full iTerm2 startup battery: version probes, option probes
  (`aggressive-resize`, `status`, `set-titles`, …), UTF-8 check, and
  the `@iterm2_*` session user options iTerm2 uses to persist window
  arrangement across detach/attach.

Deliberately not supported: panes/splits, status bar, key bindings,
configuration files, copy mode, scripting, hooks, buffers. `split-window`
returns a tmux-style error, which iTerm2 reports and tolerates.

## Wire compatibility

The compatibility target is tmux's control mode as consumed by iTerm2. Two
sources ground the implementation:

- Byte-level golden traces of `tmux -CC` (tmux 3.4): handshake ordering,
  `%begin/%end/%error` guard discipline and flags, `%output` octal escaping
  (`\134` for backslash, `\ooo` for control bytes, raw UTF-8 passthrough),
  layout checksums, notification ordering for window lifecycle.
- iTerm2's own source (TmuxGateway/TmuxController): every command it sends,
  every notification it parses, its version gating, and which commands must
  succeed vs. tolerate errors.

smux reports tmux version `3.2a`. Subscriptions (`refresh-client -B`) are
declined so iTerm2 falls back to polling; pause mode is accepted but smux
never pauses panes.

## Design

```
smux -CC  (client: dumb pipe, raw tty <-> unix socket)
              │
              ▼
   /tmp/smux-<uid>/default (0700)
              │
   smux --server  (daemon; owns sessions/windows/PTYs)
```

- All protocol logic lives in the server; the client just pumps bytes. A
  dying client (SSH drop, SIGKILL, network partition) can never take state
  with it.
- One mutex guards all server state. PTY readers, client readers, and child
  reapers serialize through it; per-client writer goroutines drain bounded
  buffers so a slow or dead connection never blocks the server or other
  clients (a client falling 64 MB behind is disconnected; the session is
  unaffected).
- Notifications triggered by a command are deferred until after its
  closing guard — iTerm2 treats any line inside a `%begin` block as
  response body.
- The server exits when its last session closes. Sessions end when their
  last shell exits or via `kill-session`/`kill-server`.
- Dependencies: `creack/pty` and `x/sys` only.

## Building and testing

```sh
go build -o smux .
go test ./...
```

The test suite includes:

- Unit tests for output escaping, layout checksums (golden values from
  tmux 3.4), command parsing, format expansion, and key encoding.
- A terminal-screen-model suite (wrapping, scrollback, SGR capture,
  alternate screen, wide chars, split UTF-8 feeds).
- A fake-iTerm2 protocol suite that replays iTerm2's real startup sequence
  and window lifecycle against a live server, including the abrupt
  disconnect → reattach → content-restore path.
- End-to-end tests of the actual binary under a PTY, including SIGKILL of
  the attached client.
- A differential test that runs the same scripted conversation against real
  tmux (when installed) and asserts the protocol skeletons are identical.
