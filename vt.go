package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// VT is a deliberately small terminal-screen model. Its only job is to
// answer capture-pane well enough that iTerm2 can repopulate a window's
// contents when a session is reattached; it is not used for live output
// (live output is forwarded verbatim to iTerm2, which does its own
// rendering). It tracks the character grid, SGR attributes, scrollback,
// cursor, scroll region, and the alternate screen. Sequences it does not
// understand are parsed and dropped.
type VT struct {
	width, height int
	histLimit     int

	screen  []vtLine // primary screen, len == height
	history []vtLine // scrolled-off lines, oldest first, capped at histLimit
	alt     []vtLine // alternate screen when active, else nil
	onAlt   bool

	cx, cy       int  // cursor
	cursorApp    bool // DECCKM: application cursor keys (affects send-keys)
	bracketPaste bool // mode 2004 (affects paste-buffer -p)
	wrapNext     bool
	savedX       int
	savedY       int
	top, bot     int // scroll region, inclusive
	attr         vtAttr
	title        string
	inUTF8       []byte // partial UTF-8 sequence spanning Feed calls
	state        int
	escBuf       []byte
	oscBuf       []byte
	stTerminal   bool // parsing string sequence terminated by ST/BEL

	// Answers owed to the program, to be written back to its pty.
	//
	// A pane's program cannot tell what it is attached to except by asking,
	// and it asks by writing a query and waiting for the terminal to type the
	// answer back. Passing the query through to the real terminal is not
	// enough: the reply would arrive as input to whatever the client decides,
	// not to the pane that asked. So the multiplexer has to answer for
	// itself, exactly as tmux does. Left unanswered, programs conclude the
	// terminal is primitive — Claude Code, for one, stops using italics and
	// underlines everything instead.
	replies []byte
}

type vtAttr struct {
	fg, bg  int32 // -1 default; 0-255 palette; 1<<24|rgb for truecolor
	bold    bool
	dim     bool
	italic  bool
	under   bool
	reverse bool
	strike  bool
}

var defaultAttr = vtAttr{fg: -1, bg: -1}

// vtVersion is what XTVERSION reports; main.go points it at the build stamp.
var vtVersion = "dev"

type vtCell struct {
	text  string // one grapheme (may include combining marks); "" = blank
	width int8
	attr  vtAttr
}

type vtLine struct {
	cells   []vtCell
	wrapped bool // continuation of the previous line (soft wrap)
}

const (
	stGround = iota
	stEsc
	stCSI
	stString // OSC/DCS/APC/PM/screen-title, consumed until ST or BEL
)

func NewVT(width, height, histLimit int) *VT {
	v := &VT{width: width, height: height, histLimit: histLimit,
		attr: defaultAttr, top: 0, bot: height - 1}
	v.screen = make([]vtLine, height)
	return v
}

func (v *VT) Resize(width, height int) {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	for height < len(v.screen) {
		// Shrink: move top lines into history (matches how shells redraw).
		if !v.onAlt {
			v.pushHistory(v.screen[0])
		}
		v.screen = v.screen[1:]
		if v.cy > 0 {
			v.cy--
		}
	}
	for len(v.screen) < height {
		v.screen = append(v.screen, vtLine{})
	}
	if v.alt != nil {
		alt := make([]vtLine, height)
		copy(alt, v.alt)
		v.alt = alt
	}
	v.width, v.height = width, height
	v.top, v.bot = 0, height-1
	v.clampCursor()
}

func (v *VT) clampCursor() {
	if v.cx > v.width-1 {
		v.cx = v.width - 1
	}
	if v.cx < 0 {
		v.cx = 0
	}
	if v.cy > v.height-1 {
		v.cy = v.height - 1
	}
	if v.cy < 0 {
		v.cy = 0
	}
}

// Feed consumes raw PTY output.
func (v *VT) Feed(data []byte) {
	if len(v.inUTF8) > 0 {
		data = append(v.inUTF8, data...)
		v.inUTF8 = nil
	}
	i := 0
	for i < len(data) {
		c := data[i]
		switch v.state {
		case stGround:
			switch {
			case c == 0x1b:
				v.state = stEsc
				v.escBuf = v.escBuf[:0]
				i++
			case c == '\r':
				v.cx, v.wrapNext = 0, false
				i++
			case c == '\n', c == 0x0b, c == 0x0c:
				v.lineFeed()
				i++
			case c == '\b':
				if v.cx > 0 {
					v.cx--
				}
				v.wrapNext = false
				i++
			case c == '\t':
				v.cx = (v.cx/8 + 1) * 8
				if v.cx > v.width-1 {
					v.cx = v.width - 1
				}
				i++
			case c < 0x20 || c == 0x7f:
				i++ // BEL, SO/SI, NUL, DEL...: ignore
			default:
				r, size := utf8.DecodeRune(data[i:])
				if r == utf8.RuneError && size == 1 {
					if !utf8.FullRune(data[i:]) && len(data)-i < 4 {
						// Partial rune at end of chunk; keep for next Feed.
						v.inUTF8 = append([]byte(nil), data[i:]...)
						return
					}
					r = 0xfffd // invalid byte: replacement char
				}
				v.print(r)
				i += size
			}
		case stEsc:
			i += v.feedEsc(c)
		case stCSI:
			v.escBuf = append(v.escBuf, c)
			if c >= 0x40 && c <= 0x7e {
				v.dispatchCSI()
				v.state = stGround
			} else if len(v.escBuf) > 128 {
				v.state = stGround // malformed; bail out
			}
			i++
		case stString:
			// Consume until BEL or ST (ESC \).
			if c == 0x07 {
				v.endString()
				v.state = stGround
			} else if c == 0x1b && i+1 < len(data) && data[i+1] == '\\' {
				v.endString()
				v.state = stGround
				i++
			} else if c == 0x1b && i+1 == len(data) {
				// ESC at chunk boundary: stash so we can see the next byte.
				v.inUTF8 = []byte{0x1b}
				return
			} else {
				if len(v.oscBuf) < 4096 {
					v.oscBuf = append(v.oscBuf, c)
				}
			}
			i++
		}
	}
}

// feedEsc handles the byte after ESC; returns bytes consumed.
func (v *VT) feedEsc(c byte) int {
	switch c {
	case '[':
		v.state = stCSI
		v.escBuf = v.escBuf[:0]
	case ']', 'P', '^', '_', 'k', 'X':
		// OSC / DCS / PM / APC / screen-style title: string sequence.
		v.state = stString
		v.oscBuf = v.oscBuf[:0]
		v.stTerminal = c == ']' || c == 'k'
	case '7':
		v.savedX, v.savedY = v.cx, v.cy
		v.state = stGround
	case '8':
		v.cx, v.cy = v.savedX, v.savedY
		v.wrapNext = false
		v.clampCursor()
		v.state = stGround
	case 'D':
		v.lineFeed()
		v.state = stGround
	case 'M':
		v.reverseIndex()
		v.state = stGround
	case 'E':
		v.cx = 0
		v.lineFeed()
		v.state = stGround
	case 'c':
		v.reset()
		v.state = stGround
	case '(', ')', '*', '+', '#':
		// Charset designation / DEC line size: one more byte, ignored.
		v.escBuf = append(v.escBuf, c)
		if len(v.escBuf) >= 2 {
			v.state = stGround
		}
	default:
		v.state = stGround // =, >, and anything else: ignore
	}
	return 1
}

// TakeReplies returns and clears anything the program is owed in answer to a
// query it made. The caller writes it to the pane's pty.
func (v *VT) TakeReplies() []byte {
	if len(v.replies) == 0 {
		return nil
	}
	out := v.replies
	v.replies = nil
	return out
}

func (v *VT) reply(s string) {
	// Bounded: a program that spams queries faster than they are drained must
	// not be able to grow this without limit.
	if len(v.replies) > 64*1024 {
		return
	}
	v.replies = append(v.replies, s...)
}

func (v *VT) endString() {
	// The only string we retain is the title (OSC 0/2 or screen ESC k).
	// Control characters are stripped: the title is embedded in protocol
	// lines and must not be able to break them.
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(v.oscBuf))
	if v.stTerminal {
		// OSC 11 background query. Programs use the answer to pick a light or
		// dark palette; unanswered, they guess, and guess wrong as often as
		// not. smux has no window of its own, so it answers for the dark
		// background its clients actually draw on.
		if s == "11;?" {
			v.reply("\x1b]11;rgb:0000/0000/0000\x1b\\")
		}
		if strings.HasPrefix(s, "0;") || strings.HasPrefix(s, "2;") {
			v.title = s[2:]
		} else if !strings.Contains(s, ";") && s != "" {
			v.title = s // ESC k title
		}
	}
	v.oscBuf = v.oscBuf[:0]
}

func (v *VT) reset() {
	v.attr = defaultAttr
	v.cx, v.cy = 0, 0
	v.wrapNext = false
	v.top, v.bot = 0, v.height-1
	v.onAlt = false
	v.alt = nil
	for i := range v.screen {
		v.screen[i] = vtLine{}
	}
}

func (v *VT) line(y int) *vtLine {
	return &v.screen[y]
}

func (v *VT) putCell(x, y int, cell vtCell) {
	l := v.line(y)
	for len(l.cells) <= x {
		l.cells = append(l.cells, vtCell{width: 1})
	}
	l.cells[x] = cell
}

func (v *VT) print(r rune) {
	w := runeWidth(r)
	if w == 0 {
		// Combining mark: attach to the previous cell.
		x, y := v.cx-1, v.cy
		if v.wrapNext {
			x = v.cx
		}
		if x >= 0 && y >= 0 && y < v.height {
			l := v.line(y)
			if x < len(l.cells) && l.cells[x].text != "" {
				l.cells[x].text += string(r)
			}
		}
		return
	}
	if v.wrapNext {
		v.cx = 0
		v.lineFeed()
		v.line(v.cy).wrapped = true
		v.wrapNext = false
	}
	if v.cx+w > v.width {
		// Wide char at right edge: wrap early.
		v.cx = 0
		v.lineFeed()
		v.line(v.cy).wrapped = true
	}
	v.putCell(v.cx, v.cy, vtCell{text: string(r), width: int8(w), attr: v.attr})
	if w == 2 {
		v.putCell(v.cx+1, v.cy, vtCell{text: "", width: 0, attr: v.attr})
	}
	v.cx += w
	if v.cx >= v.width {
		v.cx = v.width - 1
		v.wrapNext = true
	}
}

func (v *VT) lineFeed() {
	v.wrapNext = false
	if v.cy == v.bot {
		v.scrollUp(1)
	} else if v.cy < v.height-1 {
		v.cy++
	}
}

func (v *VT) reverseIndex() {
	v.wrapNext = false
	if v.cy == v.top {
		v.scrollDown(1)
	} else if v.cy > 0 {
		v.cy--
	}
}

func (v *VT) scrollUp(n int) {
	for ; n > 0; n-- {
		if v.top == 0 && !v.onAlt {
			v.pushHistory(v.screen[0])
		}
		copy(v.screen[v.top:v.bot], v.screen[v.top+1:v.bot+1])
		v.screen[v.bot] = vtLine{}
	}
}

func (v *VT) scrollDown(n int) {
	for ; n > 0; n-- {
		copy(v.screen[v.top+1:v.bot+1], v.screen[v.top:v.bot])
		v.screen[v.top] = vtLine{}
	}
}

func (v *VT) pushHistory(l vtLine) {
	v.history = append(v.history, l)
	if len(v.history) > v.histLimit {
		// Drop in chunks to avoid O(n^2) copying on sustained output.
		drop := v.histLimit / 10
		if drop < 1 {
			drop = 1
		}
		v.history = append([]vtLine(nil), v.history[drop:]...)
	}
}

func (v *VT) dispatchCSI() {
	seq := v.escBuf
	if len(seq) == 0 {
		return
	}
	final := seq[len(seq)-1]
	body := string(seq[:len(seq)-1])
	private := ""
	for len(body) > 0 && (body[0] == '?' || body[0] == '>' || body[0] == '<' || body[0] == '=') {
		private += body[:1]
		body = body[1:]
	}
	params := csiParams(body)
	p := func(i, def int) int {
		if i < len(params) && params[i] > 0 {
			return params[i]
		}
		return def
	}
	p0 := func(i, def int) int { // like p but allows 0
		if i < len(params) && params[i] >= 0 {
			return params[i]
		}
		return def
	}

	switch final {
	case 'A':
		v.cy -= p(0, 1)
		if v.cy < v.top {
			v.cy = v.top
		}
		v.wrapNext = false
	case 'B', 'e':
		v.cy += p(0, 1)
		if v.cy > v.bot {
			v.cy = v.bot
		}
		v.wrapNext = false
	case 'C', 'a':
		v.cx += p(0, 1)
		v.clampCursor()
		v.wrapNext = false
	case 'D':
		v.cx -= p(0, 1)
		v.clampCursor()
		v.wrapNext = false
	case 'E':
		v.cx = 0
		v.cy += p(0, 1)
		v.clampCursor()
		v.wrapNext = false
	case 'F':
		v.cx = 0
		v.cy -= p(0, 1)
		v.clampCursor()
		v.wrapNext = false
	case 'G', '`':
		v.cx = p(0, 1) - 1
		v.clampCursor()
		v.wrapNext = false
	case 'H', 'f':
		v.cy = p(0, 1) - 1
		v.cx = p(1, 1) - 1
		v.clampCursor()
		v.wrapNext = false
	case 'd':
		v.cy = p(0, 1) - 1
		v.clampCursor()
		v.wrapNext = false
	case 'J':
		v.eraseDisplay(p0(0, 0))
	case 'K':
		v.eraseLine(p0(0, 0))
	case 'L':
		v.insertLines(p(0, 1))
	case 'M':
		v.deleteLines(p(0, 1))
	case 'P':
		v.deleteChars(p(0, 1))
	case '@':
		v.insertChars(p(0, 1))
	case 'X':
		v.eraseChars(p(0, 1))
	case 'S':
		v.scrollUp(p(0, 1))
	case 'T':
		v.scrollDown(p(0, 1))
	case 'r':
		top, bot := p(0, 1)-1, p(1, v.height)-1
		if top < 0 {
			top = 0
		}
		if bot > v.height-1 {
			bot = v.height - 1
		}
		if top < bot {
			v.top, v.bot = top, bot
			v.cx, v.cy = 0, 0
		}
	case 'h', 'l':
		set := final == 'h'
		if private == "?" {
			for _, m := range params {
				switch m {
				case 1:
					v.cursorApp = set
				case 2004:
					v.bracketPaste = set
				case 1049, 1047, 47:
					v.setAltScreen(set)
				}
			}
		}
	case 'm':
		v.applySGR(params, body)
	case 'c':
		switch private {
		case "":
			// DA1. "VT100 with advanced video option", which is what xterm
			// and tmux both answer and what callers test for to decide they
			// are talking to something capable.
			v.reply("\x1b[?1;2c")
		case ">":
			// DA2: terminal type 84 ("xterm"), version, cartridge. Reporting
			// xterm's identity rather than inventing one keeps callers on
			// their well-tested path.
			v.reply("\x1b[>84;0;0c")
		}
	case 'n':
		if private == "" && p0(0, 0) == 6 {
			// CPR. Coordinates are 1-based on the wire.
			v.reply(fmt.Sprintf("\x1b[%d;%dR", v.cy+1, v.cx+1))
		}
	case 'q':
		if private == ">" {
			// XTVERSION.
			v.reply("\x1bP>|smux " + vtVersion + "\x1b\\")
		}
	}
}

func csiParams(body string) []int {
	if body == "" {
		return nil
	}
	// Colon sub-parameters (e.g. 38:5:196) are treated like semicolons;
	// good enough for attribute tracking.
	fields := strings.FieldsFunc(body, func(r rune) bool { return r == ';' || r == ':' })
	if strings.Contains(body, ";;") || strings.HasPrefix(body, ";") || strings.HasSuffix(body, ";") {
		// Preserve empty params as -1 so defaults apply.
		fields = strings.Split(strings.ReplaceAll(body, ":", ";"), ";")
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			out = append(out, -1)
			continue
		}
		n := 0
		valid := true
		for i := 0; i < len(f); i++ {
			if f[i] < '0' || f[i] > '9' {
				valid = false
				break
			}
			n = n*10 + int(f[i]-'0')
			if n > 1<<24 {
				n = 1 << 24
			}
		}
		if !valid {
			out = append(out, -1)
			continue
		}
		out = append(out, n)
	}
	return out
}

func (v *VT) setAltScreen(on bool) {
	if on == v.onAlt {
		return
	}
	if on {
		v.alt = v.screen
		v.screen = make([]vtLine, v.height)
		v.onAlt = true
		v.savedX, v.savedY = v.cx, v.cy
		v.cx, v.cy = 0, 0
	} else {
		v.screen = v.alt
		for len(v.screen) < v.height {
			v.screen = append(v.screen, vtLine{})
		}
		v.screen = v.screen[:v.height]
		v.alt = nil
		v.onAlt = false
		v.cx, v.cy = v.savedX, v.savedY
		v.clampCursor()
	}
	v.top, v.bot = 0, v.height-1
	v.wrapNext = false
}

func (v *VT) eraseDisplay(mode int) {
	switch mode {
	case 0:
		v.eraseLine(0)
		for y := v.cy + 1; y < v.height; y++ {
			v.screen[y] = vtLine{}
		}
	case 1:
		v.eraseLine(1)
		for y := 0; y < v.cy; y++ {
			v.screen[y] = vtLine{}
		}
	case 2, 3:
		for y := range v.screen {
			v.screen[y] = vtLine{}
		}
		if mode == 3 {
			v.history = nil
		}
	}
}

func (v *VT) eraseLine(mode int) {
	l := v.line(v.cy)
	switch mode {
	case 0:
		if v.cx < len(l.cells) {
			l.cells = l.cells[:v.cx]
		}
	case 1:
		for x := 0; x <= v.cx && x < len(l.cells); x++ {
			l.cells[x] = vtCell{text: "", width: 1, attr: v.attr}
		}
	case 2:
		l.cells = nil
	}
}

func (v *VT) insertLines(n int) {
	if v.cy < v.top || v.cy > v.bot {
		return
	}
	for ; n > 0; n-- {
		copy(v.screen[v.cy+1:v.bot+1], v.screen[v.cy:v.bot])
		v.screen[v.cy] = vtLine{}
	}
}

func (v *VT) deleteLines(n int) {
	if v.cy < v.top || v.cy > v.bot {
		return
	}
	for ; n > 0; n-- {
		copy(v.screen[v.cy:v.bot], v.screen[v.cy+1:v.bot+1])
		v.screen[v.bot] = vtLine{}
	}
}

func (v *VT) deleteChars(n int) {
	l := v.line(v.cy)
	if v.cx >= len(l.cells) {
		return
	}
	end := v.cx + n
	if end > len(l.cells) {
		end = len(l.cells)
	}
	l.cells = append(l.cells[:v.cx], l.cells[end:]...)
}

func (v *VT) insertChars(n int) {
	l := v.line(v.cy)
	if v.cx > len(l.cells) {
		return
	}
	blanks := make([]vtCell, n)
	for i := range blanks {
		blanks[i] = vtCell{text: "", width: 1, attr: v.attr}
	}
	l.cells = append(l.cells[:v.cx], append(blanks, l.cells[v.cx:]...)...)
	if len(l.cells) > v.width {
		l.cells = l.cells[:v.width]
	}
}

func (v *VT) eraseChars(n int) {
	for x := v.cx; x < v.cx+n && x < v.width; x++ {
		v.putCell(x, v.cy, vtCell{text: "", width: 1, attr: v.attr})
	}
}

func (v *VT) applySGR(params []int, body string) {
	if len(params) == 0 {
		v.attr = defaultAttr
		return
	}
	for i := 0; i < len(params); i++ {
		n := params[i]
		switch {
		case n <= 0:
			v.attr = defaultAttr
		case n == 1:
			v.attr.bold = true
		case n == 2:
			v.attr.dim = true
		case n == 3:
			v.attr.italic = true
		case n == 4:
			v.attr.under = true
		case n == 7:
			v.attr.reverse = true
		case n == 9:
			v.attr.strike = true
		case n == 21 || n == 22:
			v.attr.bold, v.attr.dim = false, false
		case n == 23:
			v.attr.italic = false
		case n == 24:
			v.attr.under = false
		case n == 27:
			v.attr.reverse = false
		case n == 29:
			v.attr.strike = false
		case n >= 30 && n <= 37:
			v.attr.fg = int32(n - 30)
		case n == 38 || n == 48:
			val, adv := parseExtColor(params[i+1:])
			if adv == 0 {
				return // malformed; drop the rest
			}
			if n == 38 {
				v.attr.fg = val
			} else {
				v.attr.bg = val
			}
			i += adv
		case n == 39:
			v.attr.fg = -1
		case n >= 40 && n <= 47:
			v.attr.bg = int32(n - 40)
		case n == 49:
			v.attr.bg = -1
		case n >= 90 && n <= 97:
			v.attr.fg = int32(n - 90 + 8)
		case n >= 100 && n <= 107:
			v.attr.bg = int32(n - 100 + 8)
		}
	}
}

// parseExtColor handles 5;n (256-color) and 2;r;g;b (truecolor) tails.
func parseExtColor(rest []int) (int32, int) {
	if len(rest) >= 2 && rest[0] == 5 && rest[1] >= 0 && rest[1] <= 255 {
		return int32(rest[1]), 2
	}
	if len(rest) >= 4 && rest[0] == 2 {
		r, g, b := rest[1], rest[2], rest[3]
		if r < 0 || g < 0 || b < 0 || r > 255 || g > 255 || b > 255 {
			return 0, 0
		}
		return int32(1<<24) | int32(r)<<16 | int32(g)<<8 | int32(b), 4
	}
	return 0, 0
}

// runeWidth is a compact wcwidth: 0 for combining marks, 2 for East Asian
// wide/fullwidth ranges and emoji, otherwise 1.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	// Combining marks.
	case (r >= 0x0300 && r <= 0x036f) ||
		(r >= 0x0483 && r <= 0x0489) ||
		(r >= 0x0591 && r <= 0x05bd) ||
		(r >= 0x0610 && r <= 0x061a) ||
		(r >= 0x064b && r <= 0x065f) ||
		(r >= 0x06d6 && r <= 0x06dc) ||
		(r >= 0x0e31 && r <= 0x0e3a && r != 0x0e32 && r != 0x0e33) ||
		(r >= 0x0e47 && r <= 0x0e4e) ||
		(r >= 0x1ab0 && r <= 0x1aff) ||
		(r >= 0x1dc0 && r <= 0x1dff) ||
		(r >= 0x20d0 && r <= 0x20f0) ||
		(r >= 0xfe00 && r <= 0xfe0f) ||
		(r >= 0xfe20 && r <= 0xfe2f) ||
		r == 0x200d:
		return 0
	// East Asian Wide / Fullwidth.
	case (r >= 0x1100 && r <= 0x115f) ||
		(r >= 0x2e80 && r <= 0x303e) ||
		(r >= 0x3041 && r <= 0x33ff) ||
		(r >= 0x3400 && r <= 0x4dbf) ||
		(r >= 0x4e00 && r <= 0x9fff) ||
		(r >= 0xa000 && r <= 0xa4cf) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe30 && r <= 0xfe4f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f64f) ||
		(r >= 0x1f900 && r <= 0x1f9ff) ||
		(r >= 0x20000 && r <= 0x2fffd) ||
		(r >= 0x30000 && r <= 0x3fffd):
		return 2
	}
	return 1
}
