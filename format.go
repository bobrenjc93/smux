package main

import (
	"strconv"
	"strings"
)

// expandFormat expands a tmux format string (-F argument): #{name}
// substitutions, #{?cond,then,else} conditionals (nestable), ## as literal
// #, and the #{==:a,b} / #{!=:a,b} comparison forms. This covers the format
// constructs iTerm2 uses; unknown variables expand to the empty string,
// which is also tmux's behavior.
func expandFormat(f string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '#' {
			b.WriteByte(f[i])
			continue
		}
		if i+1 >= len(f) {
			b.WriteByte('#')
			break
		}
		switch f[i+1] {
		case '#':
			b.WriteByte('#')
			i++
		case '{':
			body, end := matchBrace(f, i+1)
			if end < 0 {
				b.WriteByte('#')
				continue
			}
			b.WriteString(expandExpr(body, vars))
			i = end
		default:
			// Single-char shorthands (#S session name, #W window name,
			// #I window index, #P pane index, #D pane id).
			switch f[i+1] {
			case 'S':
				b.WriteString(vars["session_name"])
			case 'W':
				b.WriteString(vars["window_name"])
			case 'I':
				b.WriteString(vars["window_index"])
			case 'P':
				b.WriteString(vars["pane_index"])
			case 'D':
				b.WriteString(vars["pane_id"])
			default:
				b.WriteByte('#')
				b.WriteByte(f[i+1])
			}
			i++
		}
	}
	return b.String()
}

// matchBrace returns the content between f[open]=='{' and its matching '}'
// plus the index of the closing brace, or -1 if unbalanced.
func matchBrace(f string, open int) (string, int) {
	depth := 0
	for i := open; i < len(f); i++ {
		switch f[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return f[open+1 : i], i
			}
		}
	}
	return "", -1
}

func expandExpr(body string, vars map[string]string) string {
	switch {
	case strings.HasPrefix(body, "?"):
		parts := splitTop(body[1:], ',')
		cond := ""
		if len(parts) > 0 {
			cond = expandExpr(parts[0], vars)
		}
		branch := ""
		if truthy(cond) {
			if len(parts) > 1 {
				branch = parts[1]
			}
		} else if len(parts) > 2 {
			branch = parts[2]
		}
		return expandFormat(branch, vars)
	case strings.HasPrefix(body, "==:"), strings.HasPrefix(body, "!=:"):
		parts := splitTop(body[3:], ',')
		a, bb := "", ""
		if len(parts) > 0 {
			a = expandFormat(parts[0], vars)
		}
		if len(parts) > 1 {
			bb = expandFormat(parts[1], vars)
		}
		eq := a == bb
		if strings.HasPrefix(body, "!=:") {
			eq = !eq
		}
		if eq {
			return "1"
		}
		return "0"
	case strings.HasPrefix(body, "=") && strings.Contains(body, ":"):
		// #{=N:name}: truncate to N characters (negative: from the end).
		colon := strings.Index(body, ":")
		n, err := strconv.Atoi(body[1:colon])
		val := expandExpr(body[colon+1:], vars)
		if err != nil {
			return val
		}
		r := []rune(val)
		if n >= 0 && len(r) > n {
			return string(r[:n])
		}
		if n < 0 && len(r) > -n {
			return string(r[len(r)+n:])
		}
		return val
	default:
		// Nested constructs inside a plain reference are rare; a plain
		// variable name is the common case.
		if v, ok := vars[body]; ok {
			return v
		}
		return ""
	}
}

// splitTop splits on sep at brace depth 0.
func splitTop(s string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func truthy(s string) bool {
	return s != "" && s != "0"
}
