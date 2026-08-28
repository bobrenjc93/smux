package main

import (
	"fmt"
	"strings"
)

// captureOpts mirrors the capture-pane flags iTerm2 uses.
type captureOpts struct {
	escapes   bool // -e: include SGR escape sequences
	join      bool // -J: join wrapped lines, preserve trailing spaces
	octal     bool // -C: octal-escape non-printable characters
	noTrim    bool // -N: preserve trailing spaces
	alt       bool // -a: capture the saved (alternate) screen
	start     int  // -S: first line (0 = top of screen, negative = history)
	end       int  // -E: last line, inclusive
	haveStart bool
	haveEnd   bool
}

// capture renders pane contents like tmux capture-pane. Line 0 is the top
// of the visible screen; negative numbers index into history.
func (p *Pane) capture(o captureOpts) string {
	v := p.vt
	var all []vtLine
	var base int
	if o.alt {
		// The grid saved when the pane switched to the alternate screen
		// (caller has verified it exists). It has no history.
		all = v.alt
		base = 0
	} else {
		all = make([]vtLine, 0, len(v.history)+len(v.screen))
		all = append(all, v.history...)
		all = append(all, v.screen...)
		base = len(v.history) // index of screen line 0 within all
	}

	start, end := 0, v.height-1
	if o.haveStart {
		start = o.start
	}
	if o.haveEnd {
		end = o.end
	}
	si, ei := base+start, base+end
	if si < 0 {
		si = 0
	}
	if ei > len(all)-1 {
		ei = len(all) - 1
	}
	if si > ei {
		return ""
	}

	lines := make([]string, 0, ei-si+1)
	for i := si; i <= ei; i++ {
		text := renderLine(all[i], o)
		if o.join {
			if all[i].wrapped && len(lines) > 0 {
				lines[len(lines)-1] += text
			} else {
				lines = append(lines, text)
			}
			continue
		}
		lines = append(lines, text)
	}

	// tmux trims trailing blank lines from the capture.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func renderLine(l vtLine, o captureOpts) string {
	var b strings.Builder
	cur := defaultAttr
	// Find last non-blank cell (trailing padding is trimmed unless asked
	// not to; -J also preserves trailing spaces on wrapped lines).
	last := len(l.cells) - 1
	if !o.noTrim && !o.join {
		for last >= 0 {
			c := l.cells[last]
			if (c.text == "" || c.text == " ") && c.attr == defaultAttr {
				last--
			} else {
				break
			}
		}
	}
	for x := 0; x <= last && x < len(l.cells); x++ {
		c := l.cells[x]
		if c.width == 0 && c.text == "" {
			continue // right half of a wide char
		}
		if o.escapes && c.attr != cur {
			b.WriteString(sgrTransition(c.attr))
			cur = c.attr
		}
		text := c.text
		if text == "" {
			text = " "
		}
		if o.octal {
			for _, ch := range []byte(text) {
				if ch < 0x20 || ch == 0x7f || ch == '\\' {
					fmt.Fprintf(&b, "\\%03o", ch)
				} else {
					b.WriteByte(ch)
				}
			}
		} else {
			b.WriteString(text)
		}
	}
	if o.escapes && cur != defaultAttr {
		b.WriteString("\033[0m")
	}
	return b.String()
}

// sgrTransition emits a full SGR reset-and-set for the given attributes.
// tmux emits minimal transitions; a reset-then-set sequence is equivalent
// for any parser that tracks state (iTerm2's history parser does).
func sgrTransition(a vtAttr) string {
	params := []string{"0"}
	if a.bold {
		params = append(params, "1")
	}
	if a.dim {
		params = append(params, "2")
	}
	if a.italic {
		params = append(params, "3")
	}
	if a.under {
		params = append(params, "4")
	}
	if a.reverse {
		params = append(params, "7")
	}
	if a.strike {
		params = append(params, "9")
	}
	params = append(params, colorParams(a.fg, false)...)
	params = append(params, colorParams(a.bg, true)...)
	return "\033[" + strings.Join(params, ";") + "m"
}

func colorParams(c int32, bg bool) []string {
	if c < 0 {
		return nil
	}
	if c >= 1<<24 { // truecolor
		r := (c >> 16) & 0xff
		g := (c >> 8) & 0xff
		b := c & 0xff
		lead := "38"
		if bg {
			lead = "48"
		}
		return []string{lead, "2", fmt.Sprint(r), fmt.Sprint(g), fmt.Sprint(b)}
	}
	if c < 8 {
		base := 30
		if bg {
			base = 40
		}
		return []string{fmt.Sprint(base + int(c))}
	}
	if c < 16 {
		base := 90
		if bg {
			base = 100
		}
		return []string{fmt.Sprint(base + int(c) - 8)}
	}
	lead := "38"
	if bg {
		lead = "48"
	}
	return []string{lead, "5", fmt.Sprint(c)}
}
