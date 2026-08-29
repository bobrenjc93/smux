package main

// parseCommandLine splits one control-mode input line into commands using
// tmux's quoting rules: whitespace separates arguments; single quotes are
// literal; double quotes decode backslash escapes (see decodeQuotedEscape);
// a backslash outside quotes escapes the next character; an unquoted ';'
// terminates a command so several commands can share a line (iTerm2 sends
// such lists).
func parseCommandLine(line string) [][]string {
	var (
		cmds [][]string
		argv []string
		tok  []byte
		has  bool // current token exists (may be empty, e.g. "")
	)
	flushTok := func() {
		if has {
			argv = append(argv, string(tok))
			tok = tok[:0]
			has = false
		}
	}
	flushCmd := func() {
		flushTok()
		if len(argv) > 0 {
			cmds = append(cmds, argv)
			argv = nil
		}
	}

	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch ch {
		case ' ', '\t':
			flushTok()
		case ';':
			flushCmd()
		case '\'':
			has = true
			for i++; i < len(line) && line[i] != '\''; i++ {
				tok = append(tok, line[i])
			}
		case '"':
			has = true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) {
					decoded, adv := decodeQuotedEscape(line[i+1:])
					if adv > 0 {
						i += adv
						tok = append(tok, decoded...)
						continue
					}
				}
				tok = append(tok, line[i])
			}
		case '\\':
			has = true
			if i+1 < len(line) {
				i++
				tok = append(tok, line[i])
			}
		default:
			has = true
			tok = append(tok, ch)
		}
	}
	flushCmd()
	return cmds
}

// decodeQuotedEscape decodes the escape following a backslash inside a
// double-quoted string, tmux-style: \\ \" \$ \` \~ pass the character
// through; \n \r \t \e are C escapes; \NNN is up to three octal digits
// (dispatcher's client emits tab-separated -F formats and quoted arguments
// this way, and tmux decodes them server-side). Returns the decoded bytes
// and how many input bytes were consumed after the backslash, or (nil, 0)
// for an unrecognized escape, which the caller keeps literally.
func decodeQuotedEscape(rest string) ([]byte, int) {
	if len(rest) == 0 {
		return nil, 0
	}
	switch rest[0] {
	case '"', '\\', '$', '`', '~':
		return []byte{rest[0]}, 1
	case 'n':
		return []byte{'\n'}, 1
	case 'r':
		return []byte{'\r'}, 1
	case 't':
		return []byte{'\t'}, 1
	case 'e':
		return []byte{0x1b}, 1
	}
	if rest[0] >= '0' && rest[0] <= '7' {
		v, n := 0, 0
		for n < 3 && n < len(rest) && rest[n] >= '0' && rest[n] <= '7' {
			v = v*8 + int(rest[n]-'0')
			n++
		}
		if v <= 0xff {
			return []byte{byte(v)}, n
		}
	}
	return nil, 0
}
