package main

import (
	"reflect"
	"testing"
)

// Golden values in these tests come from byte-level traces of tmux 3.4
// (tools/traces/).

func TestEscapeOutput(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"a\tb\r\n", "a\\011b\\015\\012"},
		{"back\\slash", "back\\134slash"},
		{"\x1b[32mgreen\x1b[0m", "\\033[32mgreen\\033[0m"},
		{"é中\U0001f680", "é中\U0001f680"}, // UTF-8 passes through raw
		{"\xdb", "\xdb"},                 // invalid UTF-8 byte passes raw (tmux 3.4)
		{"\x00\x07\x7f", "\\000\\007\\177"},
	}
	for _, c := range cases {
		if got := escapeOutput([]byte(c.in)); got != c.want {
			t.Errorf("escapeOutput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLayoutChecksum(t *testing.T) {
	// tmux 3.4 reports b25d,80x24,0,0,0 for a fresh 80x24 single-pane
	// window with pane number 0, and b25e for pane number 1.
	if got := layoutChecksum("80x24,0,0,0"); got != 0xb25d {
		t.Errorf("checksum = %04x, want b25d", got)
	}
	if got := layoutChecksum("80x24,0,0,1"); got != 0xb25e {
		t.Errorf("checksum = %04x, want b25e", got)
	}
}

func TestWindowLayout(t *testing.T) {
	w := &Window{width: 80, height: 24, pane: &Pane{id: 1}}
	if got := w.layout(); got != "b25e,80x24,0,0,1" {
		t.Errorf("layout = %q", got)
	}
}

func TestParseCommandLine(t *testing.T) {
	cases := []struct {
		in   string
		want [][]string
	}{
		{`list-windows -F "#{window_id}"`, [][]string{{"list-windows", "-F", "#{window_id}"}}},
		{`send -lt %0 hello`, [][]string{{"send", "-lt", "%0", "hello"}}},
		{`new-window -PF '#{window_id}'`, [][]string{{"new-window", "-PF", "#{window_id}"}}},
		{`show -v -q -t $0 @iterm2_id; refresh-client -C 90,30`,
			[][]string{{"show", "-v", "-q", "-t", "$0", "@iterm2_id"}, {"refresh-client", "-C", "90,30"}}},
		{`rename-window -t @5 "a\\b"`, [][]string{{"rename-window", "-t", "@5", `a\b`}}},
		{`send -t %3 'C-j'`, [][]string{{"send", "-t", "%3", "C-j"}}},
		{`display -p "#{?window_active,1,0}"`, [][]string{{"display", "-p", "#{?window_active,1,0}"}}},
		{"list-sessions -F \"\t\"", [][]string{{"list-sessions", "-F", "\t"}}},
		{`a "" b`, [][]string{{"a", "", "b"}}},
		{``, nil},
		{`  ;  ; `, nil},
		{`esc\ aped`, [][]string{{"esc aped"}}},
		// Dispatcher sends \t escapes inside double-quoted -F formats and
		// expects server-side decoding to real TABs (its TSV parsing
		// depends on it).
		{`list-windows -F "#{window_id}\t#{window_name}"`,
			[][]string{{"list-windows", "-F", "#{window_id}\t#{window_name}"}}},
		// Dispatcher's quoteTmuxCommandArgument round-trip fixture:
		// ~/$HOME/"x"\y \n z \r \t ESC.
		{`rename-window "\~/\$HOME/\"x\"\\y\nz\r\t\e"`,
			[][]string{{"rename-window", "~/$HOME/\"x\"\\y\nz\r\t\x1b"}}},
		// Octal escapes for other control bytes.
		{`set-buffer -- "a\007b\033c"`,
			[][]string{{"set-buffer", "--", "a\x07b\x1bc"}}},
		// Unknown escapes stay literal.
		{`x "\q\f"`, [][]string{{"x", `\q\f`}}},
	}
	for _, c := range cases {
		if got := parseCommandLine(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCommandLine(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestExpandFormat(t *testing.T) {
	vars := map[string]string{
		"session_name":  "main",
		"window_id":     "@3",
		"window_active": "1",
		"empty":         "",
	}
	cases := []struct{ in, want string }{
		{"#{session_name}", "main"},
		{"#{window_id} #{session_name}", "@3 main"},
		{"#{?window_active,1,0}", "1"},
		{"#{?empty,yes,no}", "no"},
		{"#{?missing,yes,no}", "no"},
		{"#{unknown_var}", ""},
		{"##{literal}", "#{literal}"},
		{"#{==:#{session_name},main}", "1"},
		{"#{!=:#{session_name},main}", "0"},
		{"#{=5:session_name}", "main"},
		{"plain", "plain"},
		{"#S:#{window_id}", "main:@3"},
		{"\t", "\t"},
	}
	for _, c := range cases {
		if got := expandFormat(c.in, vars); got != c.want {
			t.Errorf("expandFormat(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	// -lt group: l is boolean, t takes the next arg.
	a := parseArgs("send-keys", []string{"-lt", "%0", "hello", "world"})
	if !a.has("l") || a.val("t") != "%0" || !reflect.DeepEqual(a.pos, []string{"hello", "world"}) {
		t.Errorf("send -lt parse: %#v", a)
	}
	// -PF with attached value.
	a = parseArgs("new-window", []string{"-PF", "#{window_id}"})
	if !a.has("P") || a.val("F") != "#{window_id}" {
		t.Errorf("new-window -PF parse: %#v", a)
	}
	// capture-pane -peqJN -t "%P" -S -2000: -S value may be negative.
	a = parseArgs("capture-pane", []string{"-peqJN", "-t", "%1", "-S", "-2000"})
	if !a.has("p") || !a.has("e") || !a.has("q") || !a.has("J") || !a.has("N") ||
		a.val("t") != "%1" || a.val("S") != "-2000" {
		t.Errorf("capture-pane parse: %#v", a)
	}
	// display -p -F fmt -t @1
	a = parseArgs("display-message", []string{"-p", "-F", "x", "-t", "@1"})
	if !a.has("p") || a.val("F") != "x" || a.val("t") != "@1" {
		t.Errorf("display parse: %#v", a)
	}
}

func TestResolveCommand(t *testing.T) {
	for in, want := range map[string]string{
		"send":         "send-keys",
		"send-keys":    "send-keys",
		"detach":       "detach-client",
		"show-option":  "show-options", // unique prefix
		"display":      "display-message",
		"set":          "set-option",
		"ls":           "list-sessions",
		"neww":         "new-window",
		"capture-pane": "capture-pane",
	} {
		name, _, err := resolveCommand(in)
		if err != nil || name != want {
			t.Errorf("resolveCommand(%q) = %q, %v; want %q", in, name, err, want)
		}
	}
	if _, _, err := resolveCommand("phony-command"); err == nil {
		t.Error("phony-command should not resolve")
	}
}

func TestKeyBytes(t *testing.T) {
	cases := []struct {
		key  string
		app  bool
		want string
	}{
		{"0x41", false, "A"},
		{"0xe9", false, "é"}, // codepoint >= 0x80 -> UTF-8
		{"0x1f680", false, "🚀"},
		{"Enter", false, "\r"},
		{"Escape", false, "\x1b"},
		{"C-c", false, "\x03"},
		{"C-Space", false, "\x00"},
		{"M-x", false, "\x1bx"},
		{"Up", false, "\x1b[A"},
		{"Up", true, "\x1bOA"},
		{"S-Up", false, "\x1b[1;2A"},
		{"BSpace", false, "\x7f"},
		{"F5", false, "\x1b[15~"},
		{"a", false, "a"},
	}
	for _, c := range cases {
		if got := string(keyBytes(c.key, c.app)); got != c.want {
			t.Errorf("keyBytes(%q, app=%v) = %q, want %q", c.key, c.app, got, c.want)
		}
	}
}
