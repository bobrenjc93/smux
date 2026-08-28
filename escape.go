package main

import (
	"fmt"
	"strings"
)

// escapeOutput encodes pane output for a %output line the way tmux does:
// backslash, all C0 control bytes, and DEL become three-digit octal escapes;
// everything else (including raw UTF-8) passes through unmodified.
//
// Verified byte-for-byte against tmux 3.4 (tools/traces/): `\` -> \134,
// 0x0d 0x0a -> \015\012, invalid high bytes pass through raw.
func escapeOutput(data []byte) string {
	var b strings.Builder
	b.Grow(len(data) + len(data)/4)
	for _, c := range data {
		if c < 0x20 || c == 0x7f || c == '\\' {
			fmt.Fprintf(&b, "\\%03o", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// layout returns the tmux window_layout string for a single-pane window:
// "<checksum>,<width>x<height>,0,0,<pane-number>".
func (w *Window) layout() string {
	body := fmt.Sprintf("%dx%d,0,0,%d", w.width, w.height, w.pane.id)
	return fmt.Sprintf("%04x,%s", layoutChecksum(body), body)
}

// layoutChecksum is tmux's layout_checksum(): a 16-bit rotate-and-add over
// the layout string (excluding the checksum prefix itself).
func layoutChecksum(body string) uint16 {
	var csum uint16
	for i := 0; i < len(body); i++ {
		csum = (csum >> 1) + ((csum & 1) << 15)
		csum += uint16(body[i])
	}
	return csum
}
