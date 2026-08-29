package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests replay the wire contract of dispatcher
// (github.com/bobrenjc93/dispatcher), a tmux -CC control-mode client, using
// the exact command strings its tmuxControlProtocol.ts builders emit.
// Unlike iTerm2, dispatcher sends \t escapes inside double-quoted -F
// formats (expecting server-side decoding), reconstructs geometry from
// list-panes instead of layout strings, and pastes via set-buffer /
// paste-buffer -p.

// Verbatim from dispatcher's tmuxControlProtocol.ts.
const (
	dispatcherWindowFmt = `"#{window_id}\t#{window_name}\t#{window_active}\t#{window_flags}\t#{host}\t#{socket_path}\t#{session_id}\t#{session_created}"`
	dispatcherPaneFmt   = `"#{window_id}\t#{pane_id}\t#{pane_left}\t#{pane_top}\t#{pane_width}\t#{pane_height}\t#{pane_active}\t#{pane_current_path}\t#{cursor_x}\t#{cursor_y}\t#{alternate_on}\t#{history_size}"`
	dispatcherCursorFmt = `"#{cursor_x}\t#{cursor_y}"`
)

func TestDispatcherHydrate(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()
	it.mustOK("refresh-client -C 80x24")

	// Full hydrate: dispatcher writes both commands back-to-back and
	// matches replies FIFO.
	it.send("list-windows -F " + dispatcherWindowFmt)
	it.send("list-panes -s -F " + dispatcherPaneFmt)

	wb, _ := it.readBlock()
	if wb.isErr || len(wb.body) != 1 {
		t.Fatalf("list-windows = %#v", wb)
	}
	wf := strings.Split(wb.body[0], "\t")
	if len(wf) != 8 {
		t.Fatalf("window snapshot has %d fields, want 8: %q", len(wf), wb.body[0])
	}
	// Dispatcher's parse requirements: window id, title, active flag "1".
	if wf[0] != "@0" || wf[1] == "" || wf[2] != "1" {
		t.Errorf("window fields: %q", wb.body[0])
	}
	// Connection-key fields (host, socket_path, session_id,
	// session_created) must all be non-empty for recycled-id protection.
	if wf[4] == "" || wf[5] == "" || wf[6] != "$0" || wf[7] == "" {
		t.Errorf("connection key fields: %q", wb.body[0])
	}

	pb, _ := it.readBlock()
	if pb.isErr || len(pb.body) != 1 {
		t.Fatalf("list-panes = %#v", pb)
	}
	pf := strings.Split(pb.body[0], "\t")
	if len(pf) != 12 {
		t.Fatalf("pane snapshot has %d fields, want 12: %q", len(pf), pb.body[0])
	}
	if pf[0] != "@0" || pf[1] != "%0" || pf[6] != "1" {
		t.Errorf("pane fields: %q", pb.body[0])
	}
	// Numeric fields must parse (dispatcher drops non-finite lines).
	for _, i := range []int{2, 3, 4, 5, 8, 9, 11} {
		if _, err := strconv.Atoi(pf[i]); err != nil {
			t.Errorf("pane field %d not numeric: %q", i, pf[i])
		}
	}
	if pf[4] != "80" || pf[5] != "24" {
		t.Errorf("pane size should match client size with no status offset: %q", pb.body[0])
	}
}

func TestDispatcherSingleWindowRefreshAndCursorProbe(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	b := it.mustOK("display-message -p -t @0 " + dispatcherWindowFmt)
	if len(b.body) != 1 || len(strings.Split(b.body[0], "\t")) != 8 {
		t.Fatalf("window refresh = %#v", b)
	}
	b = it.mustOK("list-panes -t @0 -F " + dispatcherPaneFmt)
	if len(b.body) != 1 || len(strings.Split(b.body[0], "\t")) != 12 {
		t.Fatalf("pane refresh = %#v", b)
	}
	b = it.mustOK("display-message -p -t %0 " + dispatcherCursorFmt)
	if len(b.body) != 1 {
		t.Fatalf("cursor probe = %#v", b)
	}
	parts := strings.Split(b.body[0], "\t")
	if len(parts) != 2 {
		t.Fatalf("cursor probe fields: %q", b.body[0])
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			t.Errorf("cursor field %q not numeric", p)
		}
	}
	// A refresh of a dead window must error (dispatcher treats %error and
	// empty responses as window-gone).
	it.mustErr("display-message -p -t @99 " + dispatcherWindowFmt)
}

func TestDispatcherCaptureVariants(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	it.send(`send-keys -t %0 -H 65 63 68 6f 20 44 49 53 50 2d 43 41 50 0d`) // echo DISP-CAP
	it.readBlock()
	it.captureUntil("%0", "DISP-CAP")

	// Dispatcher's three main-buffer capture forms.
	for _, cmd := range []string{
		"capture-pane -p -e -C -t %0",
		"capture-pane -p -e -C -S -2000 -t %0",
		"capture-pane -p -e -C -S -50000 -t %0",
	} {
		b := it.mustOK(cmd)
		if !strings.Contains(strings.Join(b.body, "\n"), "DISP-CAP") {
			t.Errorf("%q missing content", cmd)
		}
	}
	// Alternate screen with -q must not error when there is no alt screen.
	b := it.mustOK("capture-pane -p -e -C -a -q -t %0")
	if len(b.body) != 0 {
		t.Errorf("alt capture should be empty without alt screen: %v", b.body)
	}
}

func TestDispatcherNewWindowPlacement(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	// Two extra windows appended: order @0 @1 @2.
	it.mustOK("new-window")
	it.waitNotify("%window-add ")
	it.mustOK("new-window")
	it.waitNotify("%window-add ")

	// Dispatcher's Cmd+T: insert after the target window.
	it.mustOK("new-window -a -t @0")
	it.waitNotify("%window-add ")
	b := it.mustOK(`list-windows -F "#{window_id}"`)
	want := []string{"@0", "@3", "@1", "@2"}
	if strings.Join(b.body, " ") != strings.Join(want, " ") {
		t.Errorf("window order = %v, want %v", b.body, want)
	}
}

func TestDispatcherKillIsIdempotentFriendly(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	it.mustOK("new-window")
	it.waitNotify("%window-add ")
	// Dispatcher retries kill-window until %error (target gone = success).
	it.mustOK("kill-window -t @1")
	it.waitNotify("%unlinked-window-close ")
	it.mustErr("kill-window -t @1")
	it.mustErr("kill-pane -t %1")
}

func TestDispatcherQuotedArguments(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	// quoteTmuxCommandArgument escapes: \\ \" \$ \~ \n \r \t \e and \NNN.
	// The decoded control characters are sanitized to spaces in names.
	it.mustOK(`rename-window -t @0 "\~/\$HOME/\"x\"\\y\tz"`)
	it.waitNotify("%window-renamed ")
	b := it.mustOK(`display-message -p -t @0 "#{window_name}"`)
	if len(b.body) != 1 || b.body[0] != `~/$HOME/"x"\y z` {
		t.Errorf("decoded name = %q", b.body)
	}
}

func TestDispatcherPasteFlow(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	dir, err := os.MkdirTemp("/tmp", "smux-paste")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out")

	// Plain paste into `cat > file`: pane has not enabled bracketed paste,
	// so -p must NOT add markers.
	it.send(fmt.Sprintf(`send-keys -t %%0 -l "cat > %s" ; send-keys -t %%0 -H 0d`, out))
	it.readBlock()
	it.readBlock()
	time.Sleep(300 * time.Millisecond)

	it.mustOK(`set-buffer -b dispatcher-paste-1-0 -- "hello "`)
	it.mustOK(`set-buffer -a -b dispatcher-paste-1-0 -- "world\n"`)
	it.mustOK("paste-buffer -p -d -b dispatcher-paste-1-0 -t %0")
	// -d deleted the buffer.
	it.mustErr("paste-buffer -p -b dispatcher-paste-1-0 -t %0")

	waitFileContains(t, out, "hello world\n")
	data, _ := os.ReadFile(out)
	if strings.Contains(string(data), "\x1b[200~") {
		t.Errorf("markers added without bracketed paste mode: %q", data)
	}
	it.send("send-keys -t %0 -H 04") // C-d ends cat
	it.readBlock()

	// Bracketed paste: the pane enables mode 2004; -p must wrap the data.
	out2 := filepath.Join(dir, "out2")
	it.send(fmt.Sprintf(`send-keys -t %%0 -l "printf '\\033[?2004h'; cat > %s" ; send-keys -t %%0 -H 0d`, out2))
	it.readBlock()
	it.readBlock()
	waitForFlag(t, it, "#{bracket_paste_flag}", "1")

	// No trailing newline in the buffer: the canonical-mode tty only
	// delivers the line to cat once a newline arrives, so flush with a
	// send-keys after the paste (matching real tmux + tty behavior).
	it.mustOK(`set-buffer -b b2 -- "wrapped"`)
	it.mustOK("paste-buffer -p -d -b b2 -t %0")
	it.send("send-keys -t %0 -H 0a")
	it.readBlock()
	waitFileContains(t, out2, "\x1b[200~wrapped\x1b[201~\n")
}

func waitFileContains(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		if strings.Contains(string(data), want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("file %s never contained %q; got %q", path, want, data)
}

func waitForFlag(t *testing.T, it *iterm, format, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		it.send(fmt.Sprintf(`display-message -p -t %%0 "%s"`, format))
		b, _ := it.readBlock()
		if len(b.body) == 1 {
			last = b.body[0]
			if last == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never became %q (last %q)", format, want, last)
}

func TestDispatcherSizingContract(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	it.mustOK("new-window")
	it.waitNotify("%window-add ")

	// Dispatcher sends one client-wide size (WxH form) and expects every
	// window of the session to follow it, then %layout-change per window.
	it.mustOK("refresh-client -C 100x30")
	seen := map[string]bool{}
	for len(seen) < 2 {
		n := it.waitNotify("%layout-change ")
		parts := strings.Split(n, " ")
		if len(parts) < 3 || !strings.Contains(parts[2], "100x30") {
			t.Fatalf("layout-change = %q", n)
		}
		seen[parts[1]] = true
	}
	// Pane rows must equal client rows: no status-line offset, or
	// dispatcher enters an endless resize-correction loop.
	for _, pane := range []string{"%0", "%1"} {
		b := it.mustOK(fmt.Sprintf(`display-message -p -t %s "#{pane_width}x#{pane_height}"`, pane))
		if len(b.body) != 1 || b.body[0] != "100x30" {
			t.Errorf("pane %s size = %v, want 100x30", pane, b.body)
		}
	}

	// Pane drag on a single-pane window: accepted no-op.
	it.mustOK("resize-pane -t %0 -R 10")
}

func TestDispatcherReloadNudge(t *testing.T) {
	_, sock := startTestServer(t)
	it := dialControl(t, sock, "new")
	defer it.close()
	it.attach()

	// After an app reload dispatcher writes `list-sessions -F ''` untracked
	// and swallows the resulting block; it must be well-formed.
	b := it.mustOK("list-sessions -F ''")
	_ = b
	// The session must still work afterwards.
	it.mustOK("display-message -p -t %0 " + dispatcherCursorFmt)
}
