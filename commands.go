package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// smuxVersion is the tmux version smux reports. iTerm2 gates its feature
// set on this; 3.2a selects the modern code paths (hex send-keys, variable
// window sizes via resize-window, capture-pane -N) while avoiding the
// 3.4+/3.6+ extras (refresh-client -C @w:WxH, client trackers, clipboard
// monitors). Subscriptions and pause mode are declined at runtime, which
// iTerm2 handles by falling back to polling.
const smuxVersion = "3.2a"

type cmdFunc func(s *Server, c *Client, a *cmdArgs) (string, error)

type command struct {
	name string
	fn   cmdFunc
}

var commands = []command{
	{"attach-session", cmdAttachSession},
	{"capture-pane", cmdCapturePane},
	{"clear-history", cmdClearHistory},
	{"copy-mode", cmdNop},
	{"detach-client", cmdDetachClient},
	{"display-message", cmdDisplayMessage},
	{"has-session", cmdHasSession},
	{"kill-pane", cmdKillWindow}, // one pane per window: same thing
	{"kill-server", cmdKillServer},
	{"kill-session", cmdKillSession},
	{"kill-window", cmdKillWindow},
	{"list-clients", cmdListClients},
	{"list-keys", cmdNop},
	{"list-panes", cmdListPanes},
	{"list-sessions", cmdListSessions},
	{"list-windows", cmdListWindows},
	{"new-session", cmdNewSession},
	{"new-window", cmdNewWindow},
	{"paste-buffer", cmdPasteBuffer},
	{"refresh-client", cmdRefreshClient},
	{"rename-session", cmdRenameSession},
	{"rename-window", cmdRenameWindow},
	{"resize-pane", cmdResizeWindow},
	{"resize-window", cmdResizeWindow},
	{"select-layout", cmdNop},
	{"select-pane", cmdSelectPane},
	{"select-window", cmdSelectWindow},
	{"send-keys", cmdSendKeys},
	{"set-buffer", cmdSetBuffer},
	{"set-hook", cmdNop},
	{"set-option", cmdSetOption},
	{"set-window-option", cmdSetOption},
	{"show-options", cmdShowOptions},
	{"show-window-options", cmdShowOptions},
	{"split-window", cmdSplitWindow},
	{"swap-window", cmdSwapWindow},
	{"unlink-window", cmdKillWindow}, // windows are never multiply linked
}

var aliases = map[string]string{
	"attach":    "attach-session",
	"capturep":  "capture-pane",
	"clearhist": "clear-history",
	"detach":    "detach-client",
	"display":   "display-message",
	"has":       "has-session",
	"killp":     "kill-pane",
	"killw":     "kill-window",
	"lsc":       "list-clients",
	"lsk":       "list-keys",
	"lsp":       "list-panes",
	"ls":        "list-sessions",
	"lsw":       "list-windows",
	"new":       "new-session",
	"neww":      "new-window",
	"pasteb":    "paste-buffer",
	"refresh":   "refresh-client",
	"rename":    "rename-session",
	"renamew":   "rename-window",
	"resizep":   "resize-pane",
	"resizew":   "resize-window",
	"selectl":   "select-layout",
	"selectp":   "select-pane",
	"selectw":   "select-window",
	"send":      "send-keys",
	"set":       "set-option",
	"setb":      "set-buffer",
	"setw":      "set-window-option",
	"show":      "show-options",
	"showw":     "show-window-options",
	"splitw":    "split-window",
	"swapw":     "swap-window",
	"unlinkw":   "unlink-window",
}

func init() {
	sort.Slice(commands, func(i, j int) bool { return commands[i].name < commands[j].name })
}

// resolveCommand implements tmux's lookup: exact name, then alias, then
// unique prefix of a command name.
func resolveCommand(name string) (string, cmdFunc, error) {
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd.name, cmd.fn, nil
		}
	}
	if full, ok := aliases[name]; ok {
		return resolveCommand(full)
	}
	var matches []command
	for _, cmd := range commands {
		if strings.HasPrefix(cmd.name, name) {
			matches = append(matches, cmd)
		}
	}
	switch len(matches) {
	case 0:
		return "", nil, fmt.Errorf("parse error: unknown command: %s", name)
	case 1:
		return matches[0].name, matches[0].fn, nil
	default:
		return "", nil, fmt.Errorf("ambiguous command: %s", name)
	}
}

// cmdArgs is a parsed argv: flags (possibly with values) and positionals.
type cmdArgs struct {
	flags map[string]string
	pos   []string
}

func (a *cmdArgs) has(f string) bool   { _, ok := a.flags[f]; return ok }
func (a *cmdArgs) val(f string) string { return a.flags[f] }
func (a *cmdArgs) intVal(f string, def int) int {
	if v, ok := a.flags[f]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// valueFlags lists flags that consume a following argument, per command.
// Everything else is boolean. Unknown boolean flags are accepted and
// ignored — erroring on them would make smux more brittle than tmux across
// iTerm2 versions, and iTerm2 treats many unexpected errors as fatal.
var valueFlags = map[string]string{
	"attach-session":      "t",
	"capture-pane":        "SEtb",
	"clear-history":       "t",
	"detach-client":       "st",
	"display-message":     "tcdF",
	"has-session":         "t",
	"kill-pane":           "t",
	"kill-session":        "t",
	"kill-window":         "t",
	"list-clients":        "Ft",
	"list-panes":          "Ftf",
	"list-sessions":       "Ff",
	"list-windows":        "Ftf",
	"new-session":         "stncF",
	"new-window":          "tncF",
	"paste-buffer":        "bts",
	"refresh-client":      "CFBArftl",
	"rename-session":      "t",
	"rename-window":       "t",
	"resize-pane":         "txy",
	"resize-window":       "txy",
	"select-layout":       "t",
	"select-pane":         "tT",
	"select-window":       "t",
	"send-keys":           "tN",
	"set-buffer":          "btn",
	"set-hook":            "t",
	"set-option":          "t",
	"set-window-option":   "t",
	"show-options":        "t",
	"show-window-options": "t",
	"split-window":        "tcelF",
	"swap-window":         "st",
	"unlink-window":       "t",
}

// parseArgs parses tmux-style flags: groups like -lt where only a
// value-taking flag ends the group (taking the rest of the group or the
// next argument as its value).
func parseArgs(cmdName string, argv []string) *cmdArgs {
	vf := valueFlags[cmdName]
	a := &cmdArgs{flags: make(map[string]string)}
	i := 0
	for ; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			i++
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			break
		}
		consumedValue := false
		for j := 1; j < len(arg); j++ {
			f := arg[j : j+1]
			if strings.Contains(vf, f) {
				if j+1 < len(arg) {
					a.flags[f] = arg[j+1:]
				} else if i+1 < len(argv) {
					i++
					a.flags[f] = argv[i]
				} else {
					a.flags[f] = ""
				}
				consumedValue = true
				break
			}
			a.flags[f] = ""
		}
		_ = consumedValue
	}
	a.pos = append(a.pos, argv[i:]...)
	return a
}

// execCommand runs one command for a control client. Called with the server
// lock held; returns the response body or an error (which becomes %error).
func (s *Server) execCommand(c *Client, argv []string) (string, error) {
	full, fn, err := resolveCommand(argv[0])
	if err != nil {
		return "", err
	}
	return fn(s, c, parseArgs(full, argv[1:]))
}

// ---- target resolution ------------------------------------------------------

func (s *Server) targetSession(c *Client, t string) (*Session, error) {
	if t == "" {
		if c != nil && c.session != nil {
			return c.session, nil
		}
		return nil, fmt.Errorf("no current session")
	}
	if strings.HasPrefix(t, "%") {
		if p := s.findPaneLocked(t); p != nil {
			return p.window.session, nil
		}
	}
	if strings.HasPrefix(t, "@") {
		if w := s.findWindowLocked(t); w != nil {
			return w.session, nil
		}
	}
	if i := strings.IndexByte(t, ':'); i >= 0 {
		t = t[:i]
	}
	if sess := s.findSessionLocked(t); sess != nil {
		return sess, nil
	}
	return nil, fmt.Errorf("can't find session: %s", t)
}

func (s *Server) targetWindow(c *Client, t string) (*Window, error) {
	if t == "" {
		sess, err := s.targetSession(c, "")
		if err != nil {
			return nil, err
		}
		if sess.active == nil {
			return nil, fmt.Errorf("no current window")
		}
		return sess.active, nil
	}
	if strings.HasPrefix(t, "%") {
		if p := s.findPaneLocked(t); p != nil {
			return p.window, nil
		}
		return nil, fmt.Errorf("can't find pane: %s", t)
	}
	if w := s.findWindowLocked(t); w != nil {
		return w, nil
	}
	// "$session:@window" or "session:index" forms.
	if i := strings.IndexByte(t, ':'); i >= 0 {
		rest := t[i+1:]
		if w := s.findWindowLocked(rest); w != nil {
			return w, nil
		}
		if sess := s.findSessionLocked(t[:i]); sess != nil {
			if idx, err := strconv.Atoi(rest); err == nil && idx >= 0 && idx < len(sess.windows) {
				return sess.windows[idx], nil
			}
		}
		return nil, fmt.Errorf("can't find window: %s", t)
	}
	if sess := s.findSessionLocked(t); sess != nil && sess.active != nil {
		return sess.active, nil
	}
	return nil, fmt.Errorf("can't find window: %s", t)
}

func (s *Server) targetPane(c *Client, t string) (*Pane, error) {
	if strings.HasPrefix(t, "%") {
		if p := s.findPaneLocked(t); p != nil {
			return p, nil
		}
		return nil, fmt.Errorf("can't find pane: %s", t)
	}
	w, err := s.targetWindow(c, t)
	if err != nil {
		return nil, err
	}
	return w.pane, nil
}

// ---- format variables -------------------------------------------------------

func (s *Server) baseVars(c *Client) map[string]string {
	v := map[string]string{
		"version":             smuxVersion,
		"socket_path":         s.sock,
		"pid":                 fmt.Sprint(os.Getpid()),
		"host":                hostname(),
		"client_control_mode": "1",
	}
	if c != nil && c.session != nil {
		addSessionVars(v, s, c.session)
	}
	return v
}

func addSessionVars(v map[string]string, s *Server, sess *Session) {
	v["session_id"] = fmt.Sprintf("$%d", sess.id)
	v["session_name"] = sess.name
	v["session_windows"] = fmt.Sprint(len(sess.windows))
	v["session_created"] = fmt.Sprint(sess.created.Unix())
	v["session_activity"] = fmt.Sprint(sess.activity.Unix())
	v["session_attached"] = fmt.Sprint(s.attachedClientCountLocked(sess))
	if sess.active != nil {
		v["session_width"] = fmt.Sprint(sess.active.width)
		v["session_height"] = fmt.Sprint(sess.active.height)
	}
}

func addWindowVars(v map[string]string, s *Server, w *Window) {
	addSessionVars(v, s, w.session)
	v["window_id"] = fmt.Sprintf("@%d", w.id)
	v["window_index"] = fmt.Sprint(w.index())
	v["window_name"] = w.name
	v["window_width"] = fmt.Sprint(w.width)
	v["window_height"] = fmt.Sprint(w.height)
	v["window_layout"] = w.layout()
	v["window_visible_layout"] = w.layout()
	v["window_panes"] = "1"
	v["window_zoomed_flag"] = "0"
	v["window_active"] = boolVar(w.session.active == w)
	v["window_flags"] = w.flags()
	v["window_activity"] = fmt.Sprint(w.session.activity.Unix())
}

func addPaneVars(v map[string]string, s *Server, p *Pane) {
	addWindowVars(v, s, p.window)
	vt := p.vt
	v["pane_id"] = fmt.Sprintf("%%%d", p.id)
	v["pane_index"] = "0"
	v["pane_active"] = "1"
	v["pane_width"] = fmt.Sprint(vt.width)
	v["pane_height"] = fmt.Sprint(vt.height)
	v["pane_title"] = vt.title
	v["pane_in_mode"] = "0"
	v["pane_dead"] = "0"
	v["pane_left"] = "0"
	v["pane_top"] = "0"
	v["pane_pid"] = fmt.Sprint(p.pid())
	v["pane_current_path"] = p.currentPath()
	v["pane_current_command"] = p.currentCommand()
	v["pane_tty"] = p.tty()
	v["history_size"] = fmt.Sprint(len(vt.history))
	v["history_limit"] = fmt.Sprint(vt.histLimit)
	v["cursor_x"] = fmt.Sprint(vt.cx)
	v["cursor_y"] = fmt.Sprint(vt.cy)
	v["cursor_flag"] = "1"
	v["insert_flag"] = "0"
	v["keypad_cursor_flag"] = boolVar(vt.cursorApp)
	v["keypad_flag"] = "0"
	v["wrap_flag"] = "1"
	v["mouse_any_flag"] = "0"
	v["mouse_standard_flag"] = "0"
	v["mouse_button_flag"] = "0"
	v["mouse_all_flag"] = "0"
	v["mouse_utf8_flag"] = "0"
	v["mouse_sgr_flag"] = "0"
	v["bracket_paste_flag"] = boolVar(vt.bracketPaste)
	v["pane_key_mode"] = ""
	v["origin_flag"] = "0"
	v["alternate_on"] = boolVar(vt.onAlt)
	v["alternate_saved_x"] = fmt.Sprint(vt.savedX)
	v["alternate_saved_y"] = fmt.Sprint(vt.savedY)
	v["saved_cursor_x"] = fmt.Sprint(vt.savedX)
	v["saved_cursor_y"] = fmt.Sprint(vt.savedY)
	v["scroll_region_upper"] = fmt.Sprint(vt.top)
	v["scroll_region_lower"] = fmt.Sprint(vt.bot)
	v["pane_tabs"] = defaultTabs(vt.width)
}

func boolVar(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func defaultTabs(width int) string {
	var b strings.Builder
	for x := 8; x < width; x += 8 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		fmt.Fprint(&b, x)
	}
	return b.String()
}

// flags renders window_flags: * current, - last.
func (w *Window) flags() string {
	if w.session.active == w {
		return "*"
	}
	if w.session.last == w {
		return "-"
	}
	return ""
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// ---- listing commands -------------------------------------------------------

func cmdListSessions(s *Server, c *Client, a *cmdArgs) (string, error) {
	f := a.val("F")
	var lines []string
	for _, sess := range s.sessions {
		v := s.baseVars(nil)
		addSessionVars(v, s, sess)
		if f == "" {
			lines = append(lines, fmt.Sprintf("%s: %d windows (created %s)",
				sess.name, len(sess.windows),
				sess.created.Format("Mon Jan  2 15:04:05 2006")))
		} else {
			lines = append(lines, expandFormat(f, v))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func cmdListWindows(s *Server, c *Client, a *cmdArgs) (string, error) {
	f := a.val("F")
	var windows []*Window
	if a.has("a") {
		for _, sess := range s.sessions {
			windows = append(windows, sess.windows...)
		}
	} else {
		sess, err := s.targetSession(c, a.val("t"))
		if err != nil {
			return "", err
		}
		windows = sess.windows
	}
	var lines []string
	for _, w := range windows {
		v := s.baseVars(nil)
		addPaneVars(v, s, w.pane)
		if f == "" {
			active := ""
			if w.session.active == w {
				active = " (active)"
			}
			lines = append(lines, fmt.Sprintf("%d: %s%s (1 panes) [%dx%d] [layout %s] @%d%s",
				w.index(), w.name, w.flags(), w.width, w.height, w.layout(), w.id, active))
		} else {
			lines = append(lines, expandFormat(f, v))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func cmdListPanes(s *Server, c *Client, a *cmdArgs) (string, error) {
	f := a.val("F")
	var panes []*Pane
	switch {
	case a.has("a"):
		for _, sess := range s.sessions {
			for _, w := range sess.windows {
				panes = append(panes, w.pane)
			}
		}
	case a.has("s"):
		sess, err := s.targetSession(c, a.val("t"))
		if err != nil {
			return "", err
		}
		for _, w := range sess.windows {
			panes = append(panes, w.pane)
		}
	default:
		w, err := s.targetWindow(c, a.val("t"))
		if err != nil {
			return "", err
		}
		panes = append(panes, w.pane)
	}
	var lines []string
	for _, p := range panes {
		v := s.baseVars(nil)
		addPaneVars(v, s, p)
		if f == "" {
			lines = append(lines, fmt.Sprintf("0: [%dx%d] [history %d/%d, 0 bytes] %%%d (active)",
				p.vt.width, p.vt.height, len(p.vt.history), p.vt.histLimit, p.id))
		} else {
			lines = append(lines, expandFormat(f, v))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func cmdListClients(s *Server, c *Client, a *cmdArgs) (string, error) {
	f := a.val("F")
	var lines []string
	i := 0
	for cl := range s.clients {
		if cl.session == nil {
			continue
		}
		v := s.baseVars(cl)
		v["client_name"] = fmt.Sprintf("client%d", i)
		v["client_width"] = fmt.Sprint(cl.session.clientWidth)
		v["client_height"] = fmt.Sprint(cl.session.clientHeight)
		i++
		if f == "" {
			lines = append(lines, fmt.Sprintf("client%d: %s", i, cl.session.name))
		} else {
			lines = append(lines, expandFormat(f, v))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func cmdDisplayMessage(s *Server, c *Client, a *cmdArgs) (string, error) {
	if !a.has("p") {
		return "", nil // without -p tmux shows it on the status line: nothing to do
	}
	msg := a.val("F")
	if msg == "" && len(a.pos) > 0 {
		msg = a.pos[0]
	}
	v := s.baseVars(c)
	if t := a.val("t"); t != "" {
		// An explicit target that doesn't resolve is an error, like tmux.
		// Dispatcher relies on this to detect windows that no longer exist.
		p, err := s.targetPane(c, t)
		if err != nil {
			return "", err
		}
		addPaneVars(v, s, p)
	} else if c != nil && c.session != nil && c.session.active != nil {
		addPaneVars(v, s, c.session.active.pane)
	}
	return expandFormat(msg, v), nil
}

// ---- window/session lifecycle -----------------------------------------------

func cmdNewWindow(s *Server, c *Client, a *cmdArgs) (string, error) {
	sess, err := s.targetSession(c, a.val("t"))
	if err != nil {
		return "", err
	}
	dir := a.val("c")
	if strings.Contains(dir, "#{") {
		v := s.baseVars(nil)
		if sess.active != nil {
			addPaneVars(v, s, sess.active.pane)
		}
		dir = expandFormat(dir, v)
	}
	// -a: insert after the target window (dispatcher's Cmd+T sends
	// `new-window -a -t @N`) rather than appending at the end.
	insertAfter := -1
	if a.has("a") {
		if anchor, err := s.targetWindow(c, a.val("t")); err == nil {
			insertAfter = anchor.index()
		}
	}
	prev := sess.active
	w, err := s.newWindowLocked(sess, dir)
	if err != nil {
		return "", err
	}
	if insertAfter >= 0 && insertAfter+1 < len(sess.windows)-1 {
		ws := sess.windows
		created := ws[len(ws)-1]
		copy(ws[insertAfter+2:], ws[insertAfter+1:len(ws)-1])
		ws[insertAfter+1] = created
	}
	sess.last = prev
	s.broadcastLocked(sess, "%%session-window-changed $%d @%d", sess.id, w.id)
	s.broadcastLocked(sess, "%%window-add @%d", w.id)
	if a.has("P") {
		f := a.val("F")
		if f == "" {
			f = "#{session_name}:#{window_index}.#{pane_index}"
		}
		v := s.baseVars(nil)
		addPaneVars(v, s, w.pane)
		return expandFormat(f, v), nil
	}
	return "", nil
}

func cmdKillWindow(s *Server, c *Client, a *cmdArgs) (string, error) {
	w, err := s.targetWindow(c, a.val("t"))
	if err != nil {
		return "", err
	}
	s.destroyWindowLocked(w, false)
	return "", nil
}

func cmdKillSession(s *Server, c *Client, a *cmdArgs) (string, error) {
	sess, err := s.targetSession(c, a.val("t"))
	if err != nil {
		return "", err
	}
	s.destroySessionLocked(sess)
	return "", nil
}

func cmdKillServer(s *Server, c *Client, a *cmdArgs) (string, error) {
	for _, sess := range append([]*Session(nil), s.sessions...) {
		s.destroySessionLocked(sess)
	}
	return "", nil
}

func cmdHasSession(s *Server, c *Client, a *cmdArgs) (string, error) {
	_, err := s.targetSession(c, a.val("t"))
	return "", err
}

func cmdNewSession(s *Server, c *Client, a *cmdArgs) (string, error) {
	sess, err := s.newSessionLocked()
	if err != nil {
		return "", err
	}
	if name := a.val("s"); name != "" {
		sess.name = sanitizeName(name)
	}
	s.broadcastLocked(nil, "%%sessions-changed")
	if c != nil && !a.has("d") {
		c.session = sess
		c.notify(fmt.Sprintf("%%session-changed $%d %s", sess.id, sess.name))
	}
	return "", nil
}

func cmdAttachSession(s *Server, c *Client, a *cmdArgs) (string, error) {
	sess, err := s.targetSession(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if c != nil && c.session != sess {
		c.session = sess
		sess.activity = time.Now()
		c.notify(fmt.Sprintf("%%session-changed $%d %s", sess.id, sess.name))
	}
	return "", nil
}

func cmdRenameWindow(s *Server, c *Client, a *cmdArgs) (string, error) {
	w, err := s.targetWindow(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if len(a.pos) == 0 {
		return "", fmt.Errorf("usage: rename-window [-t target-window] new-name")
	}
	w.name = sanitizeName(a.pos[0])
	s.broadcastLocked(w.session, "%%window-renamed @%d %s", w.id, escapeWindowName(w.name))
	return "", nil
}

func cmdRenameSession(s *Server, c *Client, a *cmdArgs) (string, error) {
	sess, err := s.targetSession(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if len(a.pos) == 0 {
		return "", fmt.Errorf("usage: rename-session [-t target-session] new-name")
	}
	sess.name = sanitizeName(a.pos[0])
	s.broadcastLocked(sess, "%%session-renamed $%d %s", sess.id, sess.name)
	s.broadcastLocked(nil, "%%sessions-changed")
	return "", nil
}

// escapeWindowName escapes backslashes the way tmux does in %window-renamed
// (iTerm2 unescapes \\ -> \).
func escapeWindowName(name string) string {
	return strings.ReplaceAll(name, "\\", "\\\\")
}

// sanitizeName strips control characters (and tabs, which would corrupt the
// TSV formats iTerm2 requests) from names that get embedded in protocol
// lines.
func sanitizeName(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, name)
}

func cmdSelectWindow(s *Server, c *Client, a *cmdArgs) (string, error) {
	w, err := s.targetWindow(c, a.val("t"))
	if err != nil {
		return "", err
	}
	sess := w.session
	if sess.active != w {
		sess.last = sess.active
		sess.active = w
		sess.activity = time.Now()
		s.broadcastLocked(sess, "%%session-window-changed $%d @%d", sess.id, w.id)
	}
	return "", nil
}

func cmdSelectPane(s *Server, c *Client, a *cmdArgs) (string, error) {
	// One pane per window: selecting is a no-op, but -T sets the title.
	p, err := s.targetPane(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if a.has("T") {
		p.vt.title = a.val("T")
	}
	return "", nil
}

func cmdSwapWindow(s *Server, c *Client, a *cmdArgs) (string, error) {
	src, err := s.targetWindow(c, a.val("s"))
	if err != nil {
		return "", err
	}
	dst, err := s.targetWindow(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if src.session != dst.session {
		return "", fmt.Errorf("can't swap windows between sessions")
	}
	ws := src.session.windows
	si, di := src.index(), dst.index()
	ws[si], ws[di] = ws[di], ws[si]
	return "", nil
}

func cmdNop(s *Server, c *Client, a *cmdArgs) (string, error) {
	return "", nil
}

func cmdSplitWindow(s *Server, c *Client, a *cmdArgs) (string, error) {
	return "", fmt.Errorf("create pane failed: smux does not support split panes")
}

// ---- pane I/O ---------------------------------------------------------------

func cmdSendKeys(s *Server, c *Client, a *cmdArgs) (string, error) {
	if a.has("X") {
		return "", fmt.Errorf("not in a mode")
	}
	p, err := s.targetPane(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if p.dead {
		return "", fmt.Errorf("pane dead")
	}
	var data []byte
	switch {
	case a.has("H"):
		// Each argument is a two-digit hex byte (iTerm2 uses this for C0
		// controls on tmux >= 3.0a).
		for _, arg := range a.pos {
			n, err := strconv.ParseUint(strings.TrimPrefix(arg, "0x"), 16, 8)
			if err != nil {
				return "", fmt.Errorf("invalid key: %s", arg)
			}
			data = append(data, byte(n))
		}
	case a.has("l"):
		for i, arg := range a.pos {
			if i > 0 {
				data = append(data, ' ')
			}
			data = append(data, arg...)
		}
	default:
		for _, arg := range a.pos {
			data = append(data, keyBytes(arg, p.vt.cursorApp)...)
		}
	}
	if len(data) > 0 {
		if _, err := p.ptmx.Write(data); err != nil {
			return "", fmt.Errorf("pane write failed")
		}
	}
	p.window.session.activity = time.Now()
	return "", nil
}

func cmdCapturePane(s *Server, c *Client, a *cmdArgs) (string, error) {
	p, err := s.targetPane(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if a.has("P") {
		// Pending (unparsed) output: smux parses everything immediately.
		return "", nil
	}
	o := captureOpts{
		escapes: a.has("e"),
		join:    a.has("J"),
		octal:   a.has("C"),
		noTrim:  a.has("N"),
		alt:     a.has("a"),
	}
	if o.alt && p.vt.alt == nil {
		if a.has("q") {
			return "", nil
		}
		return "", fmt.Errorf("no alternate screen")
	}
	parseLimit := func(flag string) (int, bool) {
		val, ok := a.flags[flag]
		if !ok {
			return 0, false
		}
		if val == "-" {
			if flag == "S" {
				return -len(p.vt.history), true
			}
			return p.vt.height - 1, true
		}
		if n, err := strconv.Atoi(val); err == nil {
			return n, true
		}
		return 0, false
	}
	o.start, o.haveStart = parseLimit("S")
	o.end, o.haveEnd = parseLimit("E")
	return p.capture(o), nil
}

// cmdSetBuffer stores (or with -a appends to) a named paste buffer.
// Dispatcher chunks pastes as `set-buffer [-a] -b name -- "data"`.
func cmdSetBuffer(s *Server, c *Client, a *cmdArgs) (string, error) {
	if len(a.pos) == 0 {
		return "", fmt.Errorf("no data specified")
	}
	data := a.pos[0]
	name := a.val("b")
	if name == "" {
		name = "buffer0"
	}
	if a.has("a") {
		s.buffers[name] += data
	} else {
		s.buffers[name] = data
	}
	return "", nil
}

// cmdPasteBuffer writes a buffer into a pane. With -p, the data is wrapped
// in bracketed-paste markers if the pane's application has enabled mode
// 2004 (which is why the VT tracks it); -d deletes the buffer afterwards.
func cmdPasteBuffer(s *Server, c *Client, a *cmdArgs) (string, error) {
	name := a.val("b")
	if name == "" {
		return "", fmt.Errorf("no buffer specified")
	}
	data, ok := s.buffers[name]
	if !ok {
		return "", fmt.Errorf("no buffer %s", name)
	}
	p, err := s.targetPane(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if p.dead {
		return "", fmt.Errorf("pane dead")
	}
	if a.has("p") && p.vt.bracketPaste {
		data = "\x1b[200~" + data + "\x1b[201~"
	}
	if _, err := p.ptmx.Write([]byte(data)); err != nil {
		return "", fmt.Errorf("pane write failed")
	}
	if a.has("d") {
		delete(s.buffers, name)
	}
	p.window.session.activity = time.Now()
	return "", nil
}

func cmdClearHistory(s *Server, c *Client, a *cmdArgs) (string, error) {
	p, err := s.targetPane(c, a.val("t"))
	if err != nil {
		return "", err
	}
	p.vt.history = nil
	return "", nil
}

// ---- client/size ------------------------------------------------------------

func cmdRefreshClient(s *Server, c *Client, a *cmdArgs) (string, error) {
	if a.has("B") {
		// Declining subscriptions makes iTerm2 fall back to polling with
		// display-message, which smux answers.
		return "", fmt.Errorf("subscriptions not supported")
	}
	if cval, ok := a.flags["C"]; ok {
		w, h, err := parseSize(cval)
		if err != nil {
			return "", err
		}
		if c != nil && c.session != nil {
			sess := c.session
			sess.clientWidth, sess.clientHeight = w, h
			// Windows follow the client size until individually resized
			// (tmux window-size=latest vs. manual semantics).
			for _, win := range append([]*Window(nil), sess.windows...) {
				if !win.manualSize {
					s.resizeWindowLocked(win, w, h)
				}
			}
		}
	}
	// -f (pause-after etc.), -A (pane pause state), -l (clipboard) are
	// accepted and ignored: smux never pauses panes.
	return "", nil
}

// parseSize accepts "W,H" (what iTerm2 sends for whole-client sizing) and
// "WxH".
func parseSize(v string) (int, int, error) {
	sep := ","
	if !strings.Contains(v, ",") {
		sep = "x"
	}
	parts := strings.SplitN(v, sep, 2)
	if len(parts) == 2 {
		w, err1 := strconv.Atoi(parts[0])
		h, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && w > 0 && h > 0 {
			if w > 10000 {
				w = 10000
			}
			if h > 10000 {
				h = 10000
			}
			return w, h, nil
		}
	}
	return 0, 0, fmt.Errorf("bad size: %s", v)
}

func cmdResizeWindow(s *Server, c *Client, a *cmdArgs) (string, error) {
	w, err := s.targetWindow(c, a.val("t"))
	if err != nil {
		return "", err
	}
	if a.has("Z") {
		return "", nil // zoom toggle: single pane, nothing to do
	}
	width := a.intVal("x", w.width)
	height := a.intVal("y", w.height)
	if a.has("x") || a.has("y") {
		w.manualSize = true
	}
	s.resizeWindowLocked(w, width, height)
	return "", nil
}

func cmdDetachClient(s *Server, c *Client, a *cmdArgs) (string, error) {
	if c != nil {
		c.exit("")
	}
	return "", nil
}

// ---- options ----------------------------------------------------------------

// Options are stored at pane (-p), session (-t), or server/global scope.
// Only user options (@foo) matter to iTerm2 — it uses them to persist
// window arrangement metadata across detach/attach — but built-in names are
// stored too, and probed built-ins answer with tmux defaults.
func cmdSetOption(s *Server, c *Client, a *cmdArgs) (string, error) {
	if len(a.pos) == 0 {
		return "", fmt.Errorf("usage: set-option option [value]")
	}
	name := a.pos[0]
	value := ""
	if len(a.pos) > 1 {
		value = a.pos[1]
	}
	store, err := s.optionStore(c, a)
	if err != nil {
		return "", err
	}
	if a.has("u") {
		delete(store, name)
	} else {
		store[name] = value
	}
	return "", nil
}

func (s *Server) optionStore(c *Client, a *cmdArgs) (map[string]string, error) {
	if a.has("g") || a.has("s") {
		return s.globalOptions, nil
	}
	if a.has("p") {
		p, err := s.targetPane(c, a.val("t"))
		if err != nil {
			return nil, err
		}
		return p.options, nil
	}
	sess, err := s.targetSession(c, a.val("t"))
	if err != nil {
		// tmux resolves option scope leniently; a missing session for a
		// global-ish probe should not kill the iTerm2 connection.
		return s.globalOptions, nil
	}
	return sess.options, nil
}

func cmdShowOptions(s *Server, c *Client, a *cmdArgs) (string, error) {
	name := ""
	if len(a.pos) > 0 {
		name = a.pos[0]
	}
	if name == "" {
		return "", nil // listing all options: nothing worth reporting
	}
	value, ok := "", false
	if a.has("p") {
		if p, err := s.targetPane(c, a.val("t")); err == nil {
			value, ok = p.options[name]
		}
	} else if !a.has("g") {
		if sess, err := s.targetSession(c, a.val("t")); err == nil {
			value, ok = sess.options[name]
		}
	}
	if !ok {
		value, ok = s.globalOptions[name]
	}
	if !ok {
		value, ok = defaultOptions[name]
	}
	if !ok {
		if strings.HasPrefix(name, "@") && !a.has("q") {
			return "", fmt.Errorf("unknown option: %s", name)
		}
		return "", nil // unset: empty output, no error
	}
	if a.has("v") {
		return value, nil
	}
	return fmt.Sprintf("%s %s", name, value), nil
}

// defaultOptions answers iTerm2's startup option probes with values that
// describe smux truthfully (no status bar, no aggressive-resize).
var defaultOptions = map[string]string{
	"aggressive-resize":  "off",
	"status":             "off",
	"status-interval":    "15",
	"default-terminal":   "xterm-256color",
	"history-limit":      fmt.Sprint(scrollbackLines),
	"escape-time":        "500",
	"mouse":              "off",
	"allow-rename":       "off",
	"automatic-rename":   "on",
	"set-titles":         "off",
	"pane-border-format": "",
	"message-style":      "bg=yellow,fg=black",
}
