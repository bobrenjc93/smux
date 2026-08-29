package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	defaultWidth  = 80
	defaultHeight = 24
	// scrollbackLines is how much history each pane keeps for capture-pane
	// (content restore on reattach). tmux's default history-limit is 2000.
	scrollbackLines = 2000
)

// Session is a named collection of windows. It outlives clients.
type Session struct {
	id       int
	name     string
	windows  []*Window // ordered by window index
	active   *Window   // current window
	last     *Window   // previously current window (window_flags "-")
	created  time.Time
	activity time.Time
	// clientWidth/Height is the size reported by refresh-client -C; new
	// windows are created at this size, and windows that have not been
	// individually resized follow it (tmux window-size=latest behavior).
	clientWidth, clientHeight int
	// options holds session-scoped user options (@foo) set by iTerm2 to
	// persist its window arrangement across detach/attach.
	options map[string]string
}

// Window holds exactly one pane (smux does not support splits).
type Window struct {
	id      int
	name    string
	session *Session
	pane    *Pane
	// width/height are the window's dimensions; panes match exactly.
	width, height int
	// manualSize is set once resize-window targets this window; after
	// that, client size changes no longer affect it (tmux 3.x behavior
	// that iTerm2's per-window sizing depends on).
	manualSize bool
}

// Pane wraps a PTY running the user's shell plus a screen model used to
// answer capture-pane on reattach.
type Pane struct {
	id      int
	window  *Window
	ptmx    *os.File
	cmd     *exec.Cmd
	vt      *VT
	dead    bool
	options map[string]string // pane-scoped user options (@uservars)
}

func (s *Server) newSessionLocked() (*Session, error) {
	sess := &Session{
		id:       s.nextSessionID,
		name:     fmt.Sprintf("%d", s.nextSessionID),
		created:  time.Now(),
		activity: time.Now(),
		options:  make(map[string]string),
	}
	s.nextSessionID++
	if _, err := s.newWindowLocked(sess, ""); err != nil {
		return nil, err
	}
	s.sessions = append(s.sessions, sess)
	return sess, nil
}

func (s *Server) newWindowLocked(sess *Session, dir string) (*Window, error) {
	w := &Window{
		id:      s.nextWindowID,
		session: sess,
		width:   defaultWidth,
		height:  defaultHeight,
	}
	// Size new windows to the attached client (refresh-client -C),
	// falling back to the current window's size.
	if sess.clientWidth > 0 {
		w.width, w.height = sess.clientWidth, sess.clientHeight
	} else if sess.active != nil {
		w.width, w.height = sess.active.width, sess.active.height
	}
	s.nextWindowID++

	p := &Pane{id: s.nextPaneID, window: w, options: make(map[string]string)}
	s.nextPaneID++

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	// The server may itself have been started from inside a tmux pane;
	// pane shells must not inherit the outer multiplexer's markers.
	env := envWithout(os.Environ(),
		"TMUX", "TMUX_PANE", "STY", "WINDOW", "TERM_PROGRAM", "TERM_PROGRAM_VERSION")
	cmd.Env = append(env,
		"TERM=xterm-256color",
		fmt.Sprintf("SMUX=%s,%d", s.sock, sess.id),
		fmt.Sprintf("SMUX_PANE=%%%d", p.id),
		// Some tools change behavior when they think they are inside tmux;
		// we intentionally do NOT set TMUX to avoid claiming full tmux
		// compatibility inside the shell. SMUX marks the nesting instead.
	)
	if dir == "" {
		dir = homeDir()
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = homeDir()
	}
	cmd.Dir = dir
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(w.width), Rows: uint16(w.height),
	})
	if err != nil {
		return nil, fmt.Errorf("spawning shell: %w", err)
	}
	p.ptmx = ptmx
	p.cmd = cmd
	p.vt = NewVT(w.width, w.height, scrollbackLines)
	w.pane = p
	w.name = shellBase(shell)

	sess.windows = append(sess.windows, w)
	sess.active = w
	s.logger.Printf("window @%d pane %%%d created (pid %d) in session $%d",
		w.id, p.id, cmd.Process.Pid, sess.id)

	go s.paneReadLoop(p)
	go s.paneWaitLoop(p)
	return w, nil
}

// paneReadLoop pumps PTY output into the screen model and to attached
// control clients.
func (s *Server) paneReadLoop(p *Pane) {
	buf := make([]byte, 32*1024)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			data := buf[:n]
			s.mu.Lock()
			p.vt.Feed(data)
			p.window.session.activity = time.Now()
			line := fmt.Sprintf("%%output %%%d %s", p.id, escapeOutput(data))
			for c := range s.clients {
				if c.session == p.window.session {
					c.notify(line)
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return // EIO when the child exits; paneWaitLoop cleans up
		}
	}
}

// paneWaitLoop reaps the shell process and closes the window when it exits.
func (s *Server) paneWaitLoop(p *Pane) {
	p.cmd.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.dead {
		return
	}
	s.logger.Printf("pane %%%d process exited", p.id)
	s.destroyWindowLocked(p.window, true)
}

// destroyWindowLocked removes a window, kills its process, notifies clients,
// and destroys the session when its last window closes. When the window is
// killed by a command (rather than its process exiting) tmux unlinks it
// first and reports %unlinked-window-close; we mirror that.
func (s *Server) destroyWindowLocked(w *Window, processExited bool) {
	sess := w.session
	idx := -1
	for i, ww := range sess.windows {
		if ww == w {
			idx = i
			break
		}
	}
	if idx < 0 {
		return // already destroyed
	}
	sess.windows = append(sess.windows[:idx], sess.windows[idx+1:]...)

	if !processExited {
		killPane(w.pane)
		// paneWaitLoop reaps it; dead=true stops double destruction.
	} else {
		w.pane.dead = true
		w.pane.ptmx.Close()
	}

	if sess.last == w {
		sess.last = nil
	}
	if sess.active == w {
		switch {
		case sess.last != nil:
			sess.active = sess.last
			sess.last = nil
		case idx < len(sess.windows):
			sess.active = sess.windows[idx]
		case len(sess.windows) > 0:
			sess.active = sess.windows[len(sess.windows)-1]
		default:
			sess.active = nil
		}
		if sess.active != nil {
			s.broadcastLocked(sess, "%%session-window-changed $%d @%d",
				sess.id, sess.active.id)
		}
	}
	if processExited {
		s.broadcastLocked(sess, "%%window-close @%d", w.id)
	} else {
		s.broadcastLocked(sess, "%%unlinked-window-close @%d", w.id)
	}
	s.logger.Printf("window @%d closed", w.id)

	if len(sess.windows) == 0 {
		s.destroySessionLocked(sess)
	}
}

// destroySessionLocked tears down a session, detaching its clients.
func (s *Server) destroySessionLocked(sess *Session) {
	idx := -1
	for i, ss := range s.sessions {
		if ss == sess {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	s.sessions = append(s.sessions[:idx], s.sessions[idx+1:]...)

	for _, w := range append([]*Window(nil), sess.windows...) {
		killPane(w.pane)
	}
	sess.windows = nil
	s.logger.Printf("session $%d destroyed", sess.id)

	for c := range s.clients {
		if c.session == sess {
			c.exit("")
		}
	}
	s.broadcastLocked(nil, "%%sessions-changed")
	s.maybeExitLocked()
}

func (s *Server) findSessionLocked(target string) *Session {
	for _, sess := range s.sessions {
		if target == fmt.Sprintf("$%d", sess.id) || target == sess.name {
			return sess
		}
	}
	return nil
}

func (s *Server) findWindowLocked(target string) *Window {
	for _, sess := range s.sessions {
		for _, w := range sess.windows {
			if target == fmt.Sprintf("@%d", w.id) {
				return w
			}
		}
	}
	return nil
}

func (s *Server) findPaneLocked(target string) *Pane {
	for _, sess := range s.sessions {
		for _, w := range sess.windows {
			if target == fmt.Sprintf("%%%d", w.pane.id) {
				return w.pane
			}
		}
	}
	return nil
}

// mostRecentSessionLocked picks the session for `smux -CC a`.
func (s *Server) mostRecentSessionLocked() *Session {
	var best *Session
	for _, sess := range s.sessions {
		if best == nil || sess.activity.After(best.activity) {
			best = sess
		}
	}
	return best
}

func (w *Window) index() int {
	for i, ww := range w.session.windows {
		if ww == w {
			return i
		}
	}
	return 0
}

func (s *Server) resizeWindowLocked(w *Window, width, height int) {
	if width < 1 || height < 1 || (width == w.width && height == w.height) {
		return
	}
	w.width, w.height = width, height
	pty.Setsize(w.pane.ptmx, &pty.Winsize{Cols: uint16(width), Rows: uint16(height)})
	w.pane.vt.Resize(width, height)
	s.broadcastLocked(w.session, "%%layout-change @%d %s %s %s",
		w.id, w.layout(), w.layout(), w.flags())
}

// killPane terminates a pane's process the way tmux does: SIGHUP to the
// process group (the shell is a session leader via the pty), with closing
// the pty master as backstop. The exited process is reaped by paneWaitLoop.
func killPane(p *Pane) {
	p.dead = true
	p.ptmx.Close()
	if p.cmd.Process != nil {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGHUP)
	}
}

// envWithout returns env minus the named variables.
func envWithout(env []string, names ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		drop := false
		for _, name := range names {
			if len(kv) > len(name) && kv[len(name)] == '=' && kv[:len(name)] == name {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/"
}

func shellBase(path string) string {
	base := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			base = path[i+1:]
			break
		}
	}
	return base
}
