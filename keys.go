package main

import (
	"fmt"
	"strings"
)

// keyBytes translates a tmux key argument (as given to send-keys without -l
// or -H) into the bytes to write to the pane. It handles tmux key names,
// C-/M-/S- modifier prefixes, hex tokens (0xNN, which tmux treats as a
// Unicode code point when >= 0x80), and falls back to sending the argument
// literally — which is tmux's behavior for unrecognized multi-char strings.
// cursorApp selects SS3 encodings for arrow/home/end keys (DECCKM).
func keyBytes(key string, cursorApp bool) []byte {
	// Hex token: 0xNN.
	if strings.HasPrefix(key, "0x") || strings.HasPrefix(key, "0X") {
		var n uint32
		if _, err := fmt.Sscanf(key[2:], "%x", &n); err == nil {
			if n < 0x80 {
				return []byte{byte(n)}
			}
			return []byte(string(rune(n))) // UTF-8 encode
		}
	}

	// Modifier prefixes. S- is meaningful only for named keys; for plain
	// characters tmux just uppercases.
	ctrl, meta, shift := false, false, false
	for len(key) > 2 && key[1] == '-' {
		switch key[0] {
		case 'C', 'c':
			ctrl = true
		case 'M', 'm':
			meta = true
		case 'S', 's':
			shift = true
		default:
			goto done
		}
		key = key[2:]
	}
done:

	var out []byte
	if b, ok := namedKey(key, cursorApp, shift); ok {
		out = b
	} else if len([]rune(key)) == 1 {
		r := []rune(key)[0]
		if ctrl {
			out = []byte{ctrlByte(byte(r))}
			ctrl = false
		} else {
			out = []byte(string(r))
		}
	} else {
		// Unrecognized name: send literally (tmux errors here, but being
		// lenient can only lose us an error message).
		out = []byte(key)
	}
	if ctrl && len(out) == 1 {
		out = []byte{ctrlByte(out[0])}
	}
	if meta {
		out = append([]byte{0x1b}, out...)
	}
	return out
}

func ctrlByte(b byte) byte {
	switch {
	case b >= 'a' && b <= 'z':
		return b - 'a' + 1
	case b >= 'A' && b <= 'Z':
		return b - 'A' + 1
	case b == ' ', b == '@', b == '2':
		return 0
	case b == '[', b == '3':
		return 0x1b
	case b == '\\', b == '4':
		return 0x1c
	case b == ']', b == '5':
		return 0x1d
	case b == '^', b == '6':
		return 0x1e
	case b == '_', b == '/', b == '7', b == '?':
		return 0x1f
	}
	return b
}

func namedKey(name string, cursorApp, shift bool) ([]byte, bool) {
	arrow := func(csi, ss3 byte) []byte {
		if shift {
			return []byte(fmt.Sprintf("\033[1;2%c", csi))
		}
		if cursorApp {
			return []byte{0x1b, 'O', ss3}
		}
		return []byte{0x1b, '[', csi}
	}
	switch name {
	case "Enter":
		return []byte{'\r'}, true
	case "Escape":
		return []byte{0x1b}, true
	case "Space":
		return []byte{' '}, true
	case "Tab":
		return []byte{'\t'}, true
	case "BTab":
		return []byte("\033[Z"), true
	case "BSpace":
		return []byte{0x7f}, true
	case "DC":
		return []byte("\033[3~"), true
	case "IC":
		return []byte("\033[2~"), true
	case "Home":
		return arrow('H', 'H'), true
	case "End":
		return arrow('F', 'F'), true
	case "Up":
		return arrow('A', 'A'), true
	case "Down":
		return arrow('B', 'B'), true
	case "Right":
		return arrow('C', 'C'), true
	case "Left":
		return arrow('D', 'D'), true
	case "PPage", "PageUp", "PgUp":
		return []byte("\033[5~"), true
	case "NPage", "PageDown", "PgDn":
		return []byte("\033[6~"), true
	case "F1":
		return []byte("\033OP"), true
	case "F2":
		return []byte("\033OQ"), true
	case "F3":
		return []byte("\033OR"), true
	case "F4":
		return []byte("\033OS"), true
	case "F5":
		return []byte("\033[15~"), true
	case "F6":
		return []byte("\033[17~"), true
	case "F7":
		return []byte("\033[18~"), true
	case "F8":
		return []byte("\033[19~"), true
	case "F9":
		return []byte("\033[20~"), true
	case "F10":
		return []byte("\033[21~"), true
	case "F11":
		return []byte("\033[23~"), true
	case "F12":
		return []byte("\033[24~"), true
	}
	return nil, false
}
