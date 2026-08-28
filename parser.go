package main

// parseCommandLine splits one control-mode input line into commands using
// tmux's quoting rules: whitespace separates arguments; single quotes are
// literal; double quotes allow \" \\ escapes; a backslash outside quotes
// escapes the next character; an unquoted ';' terminates a command so
// several commands can share a line (iTerm2 sends such lists).
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
					next := line[i+1]
					if next == '"' || next == '\\' || next == '$' || next == '`' {
						i++
						tok = append(tok, next)
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
