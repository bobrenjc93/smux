package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Server owns all sessions and serves clients on a unix socket. It is the
// long-lived daemon; clients (and their SSH connections) come and go.
//
// Concurrency model: a single mutex guards all session/window/pane/client
// state. PTY readers, client readers, and child reapers all take the lock to
// mutate state and enqueue output. Per-client writer goroutines drain
// bounded queues so one stuck client can never block the server.
type Server struct {
	mu       sync.Mutex
	sock     string
	listener net.Listener
	sessions []*Session
	clients  map[*Client]bool

	nextSessionID int
	nextWindowID  int
	nextPaneID    int
	nextBlockNum  int // %begin/%end block numbering

	globalOptions map[string]string
	buffers       map[string]string // paste buffers (set-buffer/paste-buffer)

	logger *log.Logger
	done   chan struct{}
}

func runServer(sock string) {
	s, err := newServer(sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smux server: %v\n", err)
		os.Exit(1)
	}
	s.serve()
	os.Exit(0)
}

func newServer(sock string) (*Server, error) {
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Refuse to use a directory another user could tamper with.
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be mode 0700", dir)
	}

	logFile, err := os.OpenFile(filepath.Join(dir, "server.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if fi, err := logFile.Stat(); err == nil && fi.Size() > 5<<20 {
		logFile.Truncate(0)
	}

	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	os.Chmod(sock, 0o600)

	s := &Server{
		sock:          sock,
		listener:      l,
		clients:       make(map[*Client]bool),
		nextBlockNum:  100,
		globalOptions: make(map[string]string),
		buffers:       make(map[string]string),
		logger:        log.New(logFile, "", log.LstdFlags|log.Lmicroseconds),
		done:          make(chan struct{}),
	}
	s.logger.Printf("server started, pid %d, socket %s", os.Getpid(), sock)
	return s, nil
}

// serve accepts clients until the last session closes (or kill-server).
func (s *Server) serve() {
	go s.acceptLoop()
	<-s.done
	// Give clients a moment to flush their %exit before the process goes
	// away; a wedged client must not stall shutdown.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		pending := false
		for c := range s.clients {
			c.mu.Lock()
			if c.buf.Len() > 0 {
				pending = true
			}
			c.mu.Unlock()
		}
		s.mu.Unlock()
		if !pending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.logger.Printf("server exiting")
	s.listener.Close()
	os.Remove(s.sock)
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	var h hello
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		conn.Close()
		return
	}

	switch h.Cmd {
	case "oneshot":
		s.handleOneShot(conn, h.Line)
	case "new", "attach":
		c := newClient(s, conn, br)
		s.mu.Lock()
		s.clients[c] = true
		s.mu.Unlock()
		c.run(h.Cmd)
	default:
		conn.Close()
	}
}

func (s *Server) handleOneShot(conn net.Conn, line string) {
	defer conn.Close()
	s.mu.Lock()
	switch line {
	case "kill-server":
		s.logger.Printf("kill-server requested")
		for _, sess := range append([]*Session(nil), s.sessions...) {
			s.destroySessionLocked(sess)
		}
		s.mu.Unlock()
		conn.Write([]byte("server killed\n"))
		s.shutdown()
		return
	case "ls", "list-sessions":
		out := ""
		for _, sess := range s.sessions {
			attached := ""
			if s.attachedClientCountLocked(sess) > 0 {
				attached = " (attached)"
			}
			out += fmt.Sprintf("%s: %d windows (created %s)%s\n",
				sess.name, len(sess.windows),
				sess.created.Format("Mon Jan  2 15:04:05 2006"), attached)
		}
		if out == "" {
			out = "no sessions\n"
		}
		s.mu.Unlock()
		conn.Write([]byte(out))
		return
	}
	s.mu.Unlock()
	conn.Write([]byte("unknown command\n"))
}

func (s *Server) shutdown() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// maybeExitLocked shuts the server down once the last session is gone.
func (s *Server) maybeExitLocked() {
	if len(s.sessions) != 0 {
		return
	}
	s.logger.Printf("last session closed")
	// Give in-flight client writes a moment to drain, then exit — unless a
	// new session appeared in the meantime (a client racing our shutdown).
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.mu.Lock()
		empty := len(s.sessions) == 0
		s.mu.Unlock()
		if empty {
			s.shutdown()
		}
	}()
}

func (s *Server) attachedClientCountLocked(sess *Session) int {
	n := 0
	for c := range s.clients {
		if c.session == sess {
			n++
		}
	}
	return n
}

// blockNumLocked returns the next %begin/%end block number.
func (s *Server) blockNumLocked() int {
	n := s.nextBlockNum
	s.nextBlockNum++
	return n
}

// broadcastLocked sends a notification line to every client attached to
// sess (or to all clients if sess is nil).
func (s *Server) broadcastLocked(sess *Session, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	for c := range s.clients {
		if c.session == nil {
			continue // not yet attached
		}
		if sess == nil || c.session == sess {
			c.notify(line)
		}
	}
}

func (s *Server) removeClientLocked(c *Client) {
	delete(s.clients, c)
}
