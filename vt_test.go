package main

import (
	"strings"
	"testing"
)

func feedVT(t *testing.T, w, h int, input string) *VT {
	t.Helper()
	v := NewVT(w, h, 100)
	v.Feed([]byte(input))
	return v
}

func capturePlain(v *VT) string {
	p := &Pane{vt: v}
	return p.capture(captureOpts{})
}

func TestVTPlainText(t *testing.T) {
	v := feedVT(t, 20, 5, "hello\r\nworld")
	if got := capturePlain(v); got != "hello\nworld" {
		t.Errorf("capture = %q", got)
	}
	if v.cx != 5 || v.cy != 1 {
		t.Errorf("cursor = %d,%d", v.cx, v.cy)
	}
}

func TestVTWrap(t *testing.T) {
	v := feedVT(t, 10, 5, "abcdefghijKLM")
	if got := capturePlain(v); got != "abcdefghij\nKLM" {
		t.Errorf("capture = %q", got)
	}
	// -J joins the soft-wrapped line back together.
	p := &Pane{vt: v}
	if got := p.capture(captureOpts{join: true}); got != "abcdefghijKLM" {
		t.Errorf("joined capture = %q", got)
	}
}

func TestVTScrollbackAndRange(t *testing.T) {
	v := NewVT(10, 3, 100)
	v.Feed([]byte("1\r\n2\r\n3\r\n4\r\n5"))
	// Screen now shows 3,4,5; history has 1,2.
	if got := capturePlain(v); got != "3\n4\n5" {
		t.Errorf("visible = %q", got)
	}
	p := &Pane{vt: v}
	got := p.capture(captureOpts{haveStart: true, start: -2})
	if got != "1\n2\n3\n4\n5" {
		t.Errorf("with history = %q", got)
	}
	// -S clamps below the available history.
	got = p.capture(captureOpts{haveStart: true, start: -10000000})
	if got != "1\n2\n3\n4\n5" {
		t.Errorf("clamped = %q", got)
	}
}

func TestVTCarriageReturnOverwrite(t *testing.T) {
	v := feedVT(t, 20, 5, "aaaa\rbb")
	if got := capturePlain(v); got != "bbaa" {
		t.Errorf("capture = %q", got)
	}
}

func TestVTEraseAndCursorMoves(t *testing.T) {
	// Write two lines, move home, erase to end of line.
	v := feedVT(t, 20, 5, "hello world\x1b[1;6H\x1b[K")
	if got := capturePlain(v); got != "hello" {
		t.Errorf("capture = %q", got)
	}
	// ED 2: clear screen.
	v = feedVT(t, 20, 5, "junk\x1b[2Jclean\r\n")
	if got := capturePlain(v); !strings.Contains(got, "clean") || strings.Contains(got, "junk") {
		t.Errorf("capture = %q", got)
	}
}

func TestVTSGRCapture(t *testing.T) {
	v := feedVT(t, 40, 5, "\x1b[1;32mgreen\x1b[0m plain")
	p := &Pane{vt: v}
	got := p.capture(captureOpts{escapes: true})
	if !strings.Contains(got, "green") || !strings.Contains(got, "plain") {
		t.Fatalf("capture = %q", got)
	}
	if !strings.Contains(got, "\x1b[0;1;32m") {
		t.Errorf("missing bold-green SGR: %q", got)
	}
	if !strings.HasSuffix(got, "plain") {
		t.Errorf("attrs should reset before plain text: %q", got)
	}
	// 256-color and truecolor round-trip.
	v = feedVT(t, 40, 5, "\x1b[38;5;196mred\x1b[48;2;1;2;3mbg")
	got = (&Pane{vt: v}).capture(captureOpts{escapes: true})
	if !strings.Contains(got, "38;5;196") || !strings.Contains(got, "48;2;1;2;3") {
		t.Errorf("extended colors: %q", got)
	}
}

func TestVTAltScreen(t *testing.T) {
	v := feedVT(t, 20, 5, "primary\x1b[?1049halternate")
	if !v.onAlt {
		t.Fatal("should be on alt screen")
	}
	if got := capturePlain(v); got != "alternate" {
		t.Errorf("visible = %q", got)
	}
	p := &Pane{vt: v}
	if got := p.capture(captureOpts{alt: true}); got != "primary" {
		t.Errorf("saved screen = %q", got)
	}
	v.Feed([]byte("\x1b[?1049l"))
	if v.onAlt {
		t.Fatal("should be back on primary")
	}
	if got := capturePlain(v); got != "primary" {
		t.Errorf("restored = %q", got)
	}
}

func TestVTWideChars(t *testing.T) {
	v := feedVT(t, 10, 3, "中文abc")
	if got := capturePlain(v); got != "中文abc" {
		t.Errorf("capture = %q", got)
	}
	if v.cx != 7 { // 2+2+3
		t.Errorf("cx = %d", v.cx)
	}
}

func TestVTUTF8SplitAcrossFeeds(t *testing.T) {
	v := NewVT(10, 3, 10)
	b := []byte("é") // 0xc3 0xa9
	v.Feed(b[:1])
	v.Feed(b[1:])
	if got := capturePlain(v); got != "é" {
		t.Errorf("capture = %q", got)
	}
}

func TestVTScrollRegion(t *testing.T) {
	// Set region lines 1-2 (of 4), fill, and verify line 3 stays put.
	v := feedVT(t, 10, 4, "top\r\nA\r\nB\x1b[1;2r\x1b[2;1HX\r\nY\r\nZ")
	got := capturePlain(v)
	if !strings.Contains(got, "B") {
		t.Errorf("line outside region was scrolled: %q", got)
	}
}

func TestVTResize(t *testing.T) {
	v := feedVT(t, 20, 5, "one\r\ntwo\r\nthree")
	v.Resize(30, 3)
	if v.height != 3 || v.width != 30 || len(v.screen) != 3 {
		t.Fatalf("resize failed: %dx%d", v.width, v.height)
	}
	p := &Pane{vt: v}
	got := p.capture(captureOpts{haveStart: true, start: -10})
	if !strings.Contains(got, "one") || !strings.Contains(got, "three") {
		t.Errorf("content lost on resize: %q", got)
	}
}

func TestVTTitle(t *testing.T) {
	v := feedVT(t, 20, 5, "\x1b]2;my title\x07text")
	if v.title != "my title" {
		t.Errorf("title = %q", v.title)
	}
	// screen-style ESC k ... ESC \ title, as emitted by zsh setups.
	v = feedVT(t, 20, 5, "\x1bkzsh-title\x1b\\more")
	if v.title != "zsh-title" {
		t.Errorf("title = %q", v.title)
	}
	if got := capturePlain(v); got != "textmore" && got != "more" {
		// only "more" printed after the title in this feed
		if got != "more" {
			t.Errorf("capture = %q", got)
		}
	}
}

func TestVTIgnoresUnknownSequences(t *testing.T) {
	// OSC with ST, DCS, unknown CSI, mouse modes: parsed and dropped.
	v := feedVT(t, 20, 5, "\x1b]0;t\x1b\\\x1bP+q544e\x1b\\\x1b[>4;2m\x1b[?1000hok")
	if got := capturePlain(v); got != "ok" {
		t.Errorf("capture = %q", got)
	}
}

func TestVTCursorAppMode(t *testing.T) {
	v := feedVT(t, 20, 5, "\x1b[?1h")
	if !v.cursorApp {
		t.Error("DECCKM not tracked")
	}
	v.Feed([]byte("\x1b[?1l"))
	if v.cursorApp {
		t.Error("DECCKM not cleared")
	}
}

func TestVTDefaultTrimsTrailingBlanks(t *testing.T) {
	v := feedVT(t, 10, 5, "x   ")
	if got := capturePlain(v); got != "x" {
		t.Errorf("capture = %q", got)
	}
}
