package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests drive a real smux server through a fake iTerm2: they speak
// the tmux control-mode protocol over the client socket exactly the way
// iTerm2's TmuxGateway does (per its source), and assert on the replies.

func startTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	dir, err := os.MkdirTemp("", "smux-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "default")
	s, err := newServer(sock)
	if err != nil {
		t.Fatal(err)
	}
	go s.serve()
	t.Cleanup(s.shutdown)
	return s, sock
}

// iterm is a fake iTerm2 control-mode client.
type iterm struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	// partial holds an incomplete line consumed before a read timeout, so
	// timed-out reads never lose bytes.
	partial string
}

func dialControl(t *testing.T, sock, mode string) *iterm {
	it, reply := dialControlHeader(t, sock, mode)
	if !reply.OK {
		t.Fatalf("hello %q rejected: %s", mode, reply.Error)
	}
	return it
}

// dialControlHeader connects and returns the pre-flight header reply
// without failing on rejection.
func dialControlHeader(t *testing.T, sock, mode string) (*iterm, helloReply) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := json.Marshal(hello{Cmd: mode})
	if _, err := conn.Write(append(hb, '\n')); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("hello header: %v", err)
	}
	var reply helloReply
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		t.Fatalf("hello header %q: %v", line, err)
	}
	return &iterm{t: t, conn: conn, r: r}, reply
}

func (it *iterm) close() { it.conn.Close() }

// send writes a command line terminated with \r, exactly as iTerm2 does.
func (it *iterm) send(line string) {
	it.t.Helper()
	if _, err := it.conn.Write([]byte(line + "\r")); err != nil {
		it.t.Fatalf("send %q: %v", line, err)
	}
}

// readLine reads one \r\n-terminated protocol line, failing the test if
// none arrives. The deadline is generous for loaded CI runners.
func (it *iterm) readLine() string {
	it.t.Helper()
	line, err := it.readLineTimeout(30 * time.Second)
	if err != nil {
		it.t.Fatalf("readLine: %v (partial %q)", err, it.partial)
	}
	return line
}

// expectDCS consumes the \033P1000p control-mode introducer.
func (it *iterm) expectDCS() {
	it.t.Helper()
	buf := make([]byte, len("\033P1000p"))
	it.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := range buf {
		b, err := it.r.ReadByte()
		if err != nil {
			it.t.Fatalf("expectDCS: %v", err)
		}
		buf[i] = b
	}
	if string(buf) != "\033P1000p" {
		it.t.Fatalf("expected DCS introducer, got %q", buf)
	}
}

type block struct {
	body  []string
	isErr bool
}

// readBlock reads a complete %begin...%end/%error block, skipping (and
// returning) any notification lines that precede it. It validates the
// guard discipline iTerm2 enforces: matching ids, valid flags.
func (it *iterm) readBlock() (block, []string) {
	it.t.Helper()
	var notifs []string
	for {
		line := it.readLine()
		if strings.HasPrefix(line, "%begin ") {
			parts := strings.Split(line, " ")
			if len(parts) != 4 {
				it.t.Fatalf("malformed %%begin: %q", line)
			}
			ts, num := parts[1], parts[2]
			var b block
			for {
				l := it.readLine()
				if strings.HasPrefix(l, "%end ") || strings.HasPrefix(l, "%error ") {
					p := strings.Split(l, " ")
					if len(p) != 4 || p[1] != ts || p[2] != num {
						it.t.Fatalf("guard mismatch: begin %q vs %q", line, l)
					}
					b.isErr = strings.HasPrefix(l, "%error ")
					return b, notifs
				}
				b.body = append(b.body, l)
			}
		}
		if !strings.HasPrefix(line, "%") {
			it.t.Fatalf("non-notification line outside block: %q", line)
		}
		notifs = append(notifs, line)
	}
}

// mustOK sends a command and asserts the reply block is not an error.
func (it *iterm) mustOK(cmd string) block {
	it.t.Helper()
	it.send(cmd)
	b, _ := it.readBlock()
	if b.isErr {
		it.t.Fatalf("%q failed: %v", cmd, b.body)
	}
	return b
}

// mustErr sends a command and asserts the reply is an %error block.
func (it *iterm) mustErr(cmd string) block {
	it.t.Helper()
	it.send(cmd)
	b, _ := it.readBlock()
	if !b.isErr {
		it.t.Fatalf("%q should have failed, got %v", cmd, b.body)
	}
	return b
}

// attach performs the handshake: DCS, initial flags-0 block, then reads
// notifications until %session-changed. Returns the session id line.
func (it *iterm) attach() string {
	it.t.Helper()
	it.expectDCS()
	line := it.readLine()
	if !strings.HasPrefix(line, "%begin ") {
		it.t.Fatalf("expected %%begin, got %q", line)
	}
	parts := strings.Split(line, " ")
	if len(parts) != 4 || parts[3] != "0" {
		it.t.Fatalf("initial block must have flags 0: %q", line)
	}
	end := it.readLine()
	if !strings.HasPrefix(end, "%end ") {
		it.t.Fatalf("initial block failed: %q", end)
	}
	if p := strings.Split(end, " "); p[1] != parts[1] || p[2] != parts[2] {
		it.t.Fatalf("initial guard mismatch: %q vs %q", line, end)
	}
	for {
		l := it.readLine()
		if strings.HasPrefix(l, "%session-changed ") {
			return l
		}
		if !strings.HasPrefix(l, "%") {
			it.t.Fatalf("unexpected line before session-changed: %q", l)
		}
	}
}

// waitNotify reads lines (skipping others) until one matches the prefix.
func (it *iterm) waitNotify(prefix string) string {
	it.t.Helper()
	for {
		l := it.readLine()
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
}

// waitOutputContaining waits for %output lines until their decoded
// payloads, concatenated, contain the given raw bytes. Decoding mirrors
// iTerm2's TmuxGateway (\ooo octal escapes only) and is strict: raw
// control bytes or malformed escapes in a payload are protocol violations
// and fail the test. Matching on decoded bytes keeps the test robust when
// pane output is split across many %output lines.
func (it *iterm) waitOutputContaining(text string) {
	it.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var seen []byte
	for time.Now().Before(deadline) {
		l, err := it.readLineTimeout(time.Second)
		if err != nil {
			continue // idle; keep waiting until the overall deadline
		}
		if !strings.HasPrefix(l, "%output ") {
			continue
		}
		payload := l[strings.Index(l[len("%output "):], " ")+len("%output ")+1:]
		seen = append(seen, it.decodeOutput(payload)...)
		if strings.Contains(string(seen), text) {
			return
		}
	}
	it.t.Fatalf("never saw output containing %q; got: %q", text, seen)
}

// decodeOutput decodes a %output payload like iTerm2 does, failing the
// test on anything iTerm2 would mangle (raw control bytes, bad escapes).
func (it *iterm) decodeOutput(payload string) []byte {
	it.t.Helper()
	var out []byte
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if c < 0x20 || c == 0x7f {
			it.t.Fatalf("unescaped control byte %#x in %%output payload %q", c, payload)
		}
		if c != '\\' {
			out = append(out, c)
			continue
		}
		if i+3 >= len(payload) {
			it.t.Fatalf("truncated escape in %%output payload %q", payload)
		}
		v := 0
		for j := 1; j <= 3; j++ {
			d := payload[i+j]
			if d < '0' || d > '7' {
				it.t.Fatalf("malformed escape in %%output payload %q", payload)
			}
			v = v*8 + int(d-'0')
		}
		out = append(out, byte(v))
		i += 3
	}
	return out
}

// readLineTimeout reads one line, returning an error (instead of failing
// the test) if none arrives within d. A partially read line is kept for
// the next call.
func (it *iterm) readLineTimeout(d time.Duration) (string, error) {
	it.conn.SetReadDeadline(time.Now().Add(d))
	line, err := it.r.ReadString('\n')
	if err != nil {
		it.partial += line
		return "", err
	}
	line = it.partial + line
	it.partial = ""
	return strings.TrimRight(line, "\r\n"), nil
}

// captureUntil polls capture-pane until the result contains text.
func (it *iterm) captureUntil(pane, text string) string {
	it.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		it.send(fmt.Sprintf("capture-pane -peqJ -t %s -S -2000", pane))
		b, _ := it.readBlock()
		last = strings.Join(b.body, "\n")
		if strings.Contains(last, text) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	it.t.Fatalf("capture never contained %q; last: %q", text, last)
	return ""
}

func TestHandshakeNewSession(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	sc := it.attach()
	if sc != "%session-changed $0 0" {
		t.Errorf("session-changed = %q", sc)
	}
}

func TestAttachWithNoSessionsFails(t *testing.T) {
	_, sock := startTestServer(t)
	it, reply := dialControlHeader(t, sock, "attach")
	defer it.close()
	if reply.OK || !strings.Contains(reply.Error, "no session") {
		t.Errorf("attach header = %+v", reply)
	}
}

// TestSingleSessionEnforced: smux allows at most one session. A second
// `smux -CC` must be rejected before control mode starts, pointing the
// user at attach; a concurrent attach still works.
func TestSingleSessionEnforced(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	it2, reply := dialControlHeader(t, sock, "new")
	it2.close()
	if reply.OK || !strings.Contains(reply.Error, "already exists") ||
		!strings.Contains(reply.Error, "smux -CC a") {
		t.Errorf("second new header = %+v", reply)
	}

	// The in-protocol path is also guarded (covers the pre-flight race).
	it.mustErr(`new-session -s extra`)

	it3 := dialControl(t, sock, "attach")
	defer it3.close()
	if sc := it3.attach(); sc != "%session-changed $0 0" {
		t.Errorf("attach session-changed = %q", sc)
	}
}

// TestStartupBattery replays iTerm2's full startup command sequence (from
// iTerm2's TmuxController source) and asserts every command that iTerm2
// requires to succeed does succeed.
func TestStartupBattery(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	// The ^C anti-RCE byte iTerm2 writes merges with the first command
	// line; the whole line must produce exactly one %error.
	it.conn.Write([]byte("\x03"))
	it.mustErr("phony-command")

	it.mustOK("refresh-client -fpause-after=0,wait-exit")

	b := it.mustOK("show-window-options -g aggressive-resize")
	if len(b.body) > 0 && strings.Contains(b.body[0], "aggressive-resize on") {
		t.Fatal("aggressive-resize must not be on")
	}
	b = it.mustOK("show-option -g -v status")
	if len(b.body) != 1 || (b.body[0] != "on" && b.body[0] != "off") {
		t.Fatalf("status = %v", b.body)
	}
	// UTF-8 check: the literal-TAB format must come back as a raw TAB.
	it.send("list-sessions -F \"\t\"")
	b, _ = it.readBlock()
	if b.isErr || len(b.body) != 1 || b.body[0] != "\t" {
		t.Fatalf("UTF-8 check reply = %#v", b)
	}
	it.mustOK("show-options -v -s default-terminal")
	it.mustOK("list-keys")
	it.mustOK("copy-mode -q")

	b = it.mustOK(`display-message -p "#{version}"`)
	if len(b.body) != 1 || b.body[0] != smuxVersion {
		t.Fatalf("version = %v", b.body)
	}
	it.mustOK("show-window-options pane-border-format")
	it.mustOK(`list-windows -F "#{socket_path}"`)
	b = it.mustOK(`list-windows -F "#{pid}"`)
	if len(b.body) != 1 || b.body[0] == "" {
		t.Fatalf("pid probe = %v", b.body)
	}
	it.mustOK("show-options -g message-style")
	it.mustOK("refresh-client -fpause-after=120")
	b = it.mustOK(`display-message -p "#{pid}"`)
	if len(b.body) != 1 || b.body[0] == "" {
		t.Fatalf("pid = %v", b.body)
	}
	b = it.mustOK("show-options -v -g set-titles")
	if len(b.body) != 1 {
		t.Fatalf("set-titles = %v", b.body)
	}
	b = it.mustOK("show -v -q -t $0 @iterm2_size")
	if len(b.body) != 0 {
		t.Fatalf("@iterm2_size should be unset, got %v", b.body)
	}

	// Subscriptions must be declined so iTerm2 falls back to polling.
	it.mustErr(`refresh-client -B "it2_1:%0:#{pane_title}"`)

	// The big attach command list: one block per sub-command, in order.
	it.send(`show -v -q -t $0 @iterm2_id; refresh-client -C 90,30; show -v -q -t $0 @hidden; show -v -q -t $0 @buried_indexes; show -v -q -t $0 @affinities; show -v -q -t $0 @per_window_settings; show -v -q -t $0 @per_tab_settings; show -v -q -t $0 @origins; show -v -q -t $0 @hotkeys; show -v -q -t $0 @tab_colors; list-sessions -F "#{session_id} #{session_name}"; list-windows -F "#{session_name}` + "\t" + `#{window_id}` + "\t" + `#{window_name}` + "\t" + `#{window_width}` + "\t" + `#{window_height}` + "\t" + `#{window_layout}` + "\t" + `#{window_flags}` + "\t" + `#{?window_active,1,0}` + "\t" + `#{window_visible_layout}` + "\t" + `#{pane-border-status}"`)
	for i := 0; i < 10; i++ {
		b, _ := it.readBlock()
		if b.isErr {
			t.Fatalf("sub-command %d errored: %v", i, b.body)
		}
	}
	b, _ = it.readBlock() // list-sessions
	if b.isErr || len(b.body) != 1 || b.body[0] != "$0 0" {
		t.Fatalf("list-sessions = %#v", b)
	}
	b, _ = it.readBlock() // list-windows TSV
	if b.isErr || len(b.body) != 1 {
		t.Fatalf("list-windows = %#v", b)
	}
	fields := strings.Split(b.body[0], "\t")
	if len(fields) != 10 {
		t.Fatalf("list-windows TSV has %d fields, want 10: %q", len(fields), b.body[0])
	}
	if fields[1] != "@0" || fields[3] != "90" || fields[4] != "30" || fields[7] != "1" {
		t.Errorf("TSV fields: %q", b.body[0])
	}
	if !strings.Contains(fields[5], "90x30,0,0,0") {
		t.Errorf("layout: %q", fields[5])
	}

	// @iterm2_id persistence.
	it.mustOK(`set -t $0 @iterm2_id "guid-abc"`)
	b = it.mustOK("show -v -q -t $0 @iterm2_id")
	if len(b.body) != 1 || b.body[0] != "guid-abc" {
		t.Fatalf("@iterm2_id round-trip = %v", b.body)
	}

	// Per-window content restore commands (TmuxWindowOpener).
	it.send(`capture-pane -peqJN -t "%0" -S -2000; capture-pane -peqJN -a -t "%0" -S -2000; list-panes -t "%0" -F "pane_id=#{pane_id}` + "\t" + `alternate_on=#{alternate_on}` + "\t" + `cursor_x=#{cursor_x}` + "\t" + `cursor_y=#{cursor_y}"; capture-pane -p -P -C -t "%0"; refresh-client -A "%0:continue"; show-options -v -q -p -t %0 @uservars`)
	for i := 0; i < 6; i++ {
		b, _ := it.readBlock()
		if b.isErr {
			t.Fatalf("window-open command %d errored: %v", i, b.body)
		}
		if i == 2 { // list-panes state
			if len(b.body) != 1 || !strings.HasPrefix(b.body[0], "pane_id=%0\t") {
				t.Fatalf("state = %#v", b.body)
			}
		}
	}
}

func TestCommandListStopsAfterError(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	// iTerm2's FIFO accounting requires that after an error in a
	// `;`-joined list, the remaining sub-commands produce no blocks.
	it.send(`list-keys; bogus-command; list-keys`)
	b, _ := it.readBlock()
	if b.isErr {
		t.Fatal("first sub-command should succeed")
	}
	b, _ = it.readBlock()
	if !b.isErr {
		t.Fatal("second sub-command should error")
	}
	// No third block: the next command's block must be the very next one.
	b = it.mustOK("list-keys")
	_ = b
}

func TestNewWindowLifecycle(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()
	it.mustOK("refresh-client -C 80,24")

	// Cmd+T: reply must be exactly the window id; %window-add must arrive
	// after the block, preceded by %session-window-changed.
	it.send(`new-window -PF "#{window_id}"`)
	b, _ := it.readBlock()
	if b.isErr || len(b.body) != 1 || b.body[0] != "@1" {
		t.Fatalf("new-window reply = %#v", b)
	}
	if n := it.waitNotify("%session-window-changed "); n != "%session-window-changed $0 @1" {
		t.Errorf("session-window-changed = %q", n)
	}
	if n := it.waitNotify("%window-add "); n != "%window-add @1" {
		t.Errorf("window-add = %q", n)
	}

	// iTerm2's follow-up info request for the new window.
	b = it.mustOK(`display -p -F "#{window_id}` + "\t" + `#{window_width}` + "\t" + `#{window_height}` + "\t" + `#{?window_active,1,0}" -t @1`)
	if len(b.body) != 1 || b.body[0] != "@1\t80\t24\t1" {
		t.Fatalf("window info = %v", b.body)
	}

	// Close the tab.
	it.mustOK("kill-window -t @1")
	if n := it.waitNotify("%session-window-changed "); n != "%session-window-changed $0 @0" {
		t.Errorf("after kill: %q", n)
	}
	if n := it.waitNotify("%unlinked-window-close "); n != "%unlinked-window-close @1" {
		t.Errorf("window-close = %q", n)
	}
}

func TestOutputEscapingOnTheWire(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	// printf a tab, a backslash, and CR LF through the shell. The decoder
	// in waitOutputContaining verifies they arrived escaped (\011, \134,
	// \015\012) and rejects any raw control byte on the wire.
	it.send(`send -t %0 -l "printf 'X\\tY\\\\Z\\n'" ; send -t %0 0xd`)
	it.readBlock()
	it.readBlock()
	it.waitOutputContaining("X\tY\\Z\r\n")
}

func TestSendKeysForms(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	// All three iTerm2 encodings in one list: literal, hex codepoints
	// (including >0x7f), and -H raw bytes for the trailing newline.
	it.send(`send -lt %0 echo ; send -t %0 0x20 ; send -t %0 0x4d 0x41 0x52 0x4b 0xe9 ; send -H -t %0 0d`)
	for i := 0; i < 4; i++ {
		b, _ := it.readBlock()
		if b.isErr {
			t.Fatalf("send form %d failed: %v", i, b.body)
		}
	}
	it.captureUntil("%0", "MARKé")
}

func TestDetach(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	it.send("detach")
	b, _ := it.readBlock()
	if b.isErr {
		t.Fatalf("detach errored: %v", b.body)
	}
	if l := it.waitNotify("%exit"); l != "%exit" {
		t.Errorf("exit = %q", l)
	}
	// DCS terminator then EOF.
	it.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	rest := make([]byte, 16)
	n, _ := it.r.Read(rest)
	if string(rest[:n]) != "\033\\" {
		t.Errorf("trailer = %q", rest[:n])
	}
}

// TestDisconnectAndReattach is the core reliability scenario: an abrupt
// connection drop (SSH dying) must leave the session and its processes
// intact, and a later attach must restore content via capture-pane.
func TestDisconnectAndReattach(t *testing.T) {
	srv, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	it.attach()
	it.mustOK("refresh-client -C 80,24")

	// Run a marker command and a long-lived background process.
	it.send(`send -t %0 -l "echo MARKER-A11Y; sleep 500 &" ; send -H -t %0 0d`)
	it.readBlock()
	it.readBlock()
	it.captureUntil("%0", "MARKER-A11Y")

	// Grab the shell PID so we can prove it survives.
	b := it.mustOK(`display-message -p -t %0 "#{pane_pid}"`)
	if len(b.body) != 1 {
		t.Fatalf("pane_pid = %v", b.body)
	}
	var shellPid int
	fmt.Sscanf(b.body[0], "%d", &shellPid)
	if shellPid <= 0 {
		t.Fatalf("bad pid %q", b.body[0])
	}

	// Abrupt disconnect: no detach command, just a dead socket.
	it.close()
	time.Sleep(300 * time.Millisecond)

	// Session must still exist server-side with a live shell.
	if err := syscall.Kill(shellPid, 0); err != nil {
		t.Fatalf("shell died on disconnect: %v", err)
	}
	srv.mu.Lock()
	nsess := len(srv.sessions)
	srv.mu.Unlock()
	if nsess != 1 {
		t.Fatalf("session count after disconnect = %d", nsess)
	}

	// Reattach and restore.
	it2 := dialControl(t, sock, "attach")
	defer it2.close()
	sc := it2.attach()
	if sc != "%session-changed $0 0" {
		t.Errorf("reattach session-changed = %q", sc)
	}
	content := it2.captureUntil("%0", "MARKER-A11Y")
	if !strings.Contains(content, "sleep 500") {
		t.Errorf("restored content missing command line: %q", content)
	}
	if err := syscall.Kill(shellPid, 0); err != nil {
		t.Fatalf("shell died across reattach: %v", err)
	}

	// New output after reattach still flows.
	it2.send(`send -t %0 -l "echo AFTER-REATTACH" ; send -H -t %0 0d`)
	it2.readBlock()
	it2.readBlock()
	it2.waitOutputContaining("AFTER-REATTACH")
}

// TestTwoClientsSameSession: a second attach sees the same session and
// receives the same notifications (e.g. window-add from the other client).
func TestTwoClientsSameSession(t *testing.T) {
	_, sock := startTestServer(t)
	a := dialControl(t, sock, "new")
	defer a.close()
	a.attach()
	b := dialControl(t, sock, "attach")
	defer b.close()
	b.attach()

	a.send(`new-window -PF "#{window_id}"`)
	blk, _ := a.readBlock()
	if blk.isErr {
		t.Fatalf("new-window: %v", blk.body)
	}
	if n := b.waitNotify("%window-add "); n != "%window-add @1" {
		t.Errorf("second client window-add = %q", n)
	}
}

func TestShellExitClosesWindowAndSession(t *testing.T) {
	srv, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	// Open a second window, then exit the shell in it.
	it.send(`new-window -PF "#{window_id}"`)
	b, _ := it.readBlock()
	if b.isErr {
		t.Fatal("new-window failed")
	}
	it.send(`send -t %1 -l exit ; send -H -t %1 0d`)
	it.readBlock()
	it.readBlock()
	if n := it.waitNotify("%window-close "); n != "%window-close @1" {
		t.Errorf("window-close = %q", n)
	}

	// Exit the last shell: session dies, client gets %exit, server stops.
	it.send(`send -t %0 -l exit ; send -H -t %0 0d`)
	it.readBlock()
	it.readBlock()
	it.waitNotify("%exit")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-srv.done:
			return
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Fatal("server did not exit after last session closed")
}

func TestKillServerOneShot(t *testing.T) {
	srv, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := json.Marshal(hello{Cmd: "oneshot", Line: "kill-server"})
	conn.Write(append(hb, '\n'))
	conn.Close()

	it.waitNotify("%exit")
	select {
	case <-srv.done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestGarbageInputDoesNotWedgeSession(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	it.mustErr("complete garbage ' with weird quotes")
	it.mustErr("\x01\x02\x03binary")
	b := it.mustOK(`display-message -p "#{version}"`)
	if len(b.body) != 1 || b.body[0] != smuxVersion {
		t.Fatalf("session wedged after garbage: %v", b.body)
	}
}

func TestWindowRenameNotification(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	it.mustOK(`rename-window -t @0 "my tab"`)
	if n := it.waitNotify("%window-renamed "); n != "%window-renamed @0 my tab" {
		t.Errorf("window-renamed = %q", n)
	}
	b := it.mustOK(`display-message -p -t @0 "#{window_name}"`)
	if len(b.body) != 1 || b.body[0] != "my tab" {
		t.Errorf("window_name = %v", b.body)
	}
}

func TestResizeEmitsLayoutChange(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	it.mustOK("resize-window -x 120 -y 40 -t @0")
	n := it.waitNotify("%layout-change ")
	if !strings.HasPrefix(n, "%layout-change @0 ") || !strings.Contains(n, "120x40,0,0,0") {
		t.Errorf("layout-change = %q", n)
	}
	// A manually sized window no longer follows client size.
	it.mustOK("refresh-client -C 80,24")
	b := it.mustOK(`display-message -p -t @0 "#{window_width}x#{window_height}"`)
	if len(b.body) != 1 || b.body[0] != "120x40" {
		t.Errorf("manual size not sticky: %v", b.body)
	}
}
