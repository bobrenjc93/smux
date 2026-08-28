package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"sync"
	"time"
)

// maxClientBacklog is the outbound buffer ceiling per client. A client that
// falls this far behind (dead SSH link that TCP hasn't noticed yet) is
// disconnected rather than letting the server grow without bound. The
// session itself is unaffected.
const maxClientBacklog = 64 << 20

// Client is one attached control-mode connection (one `smux -CC` process,
// i.e. one iTerm2 window group). All protocol output for a client flows
// through its buffer; a per-client writer goroutine drains it so a slow or
// dead connection never blocks the server.
type Client struct {
	server  *Server
	conn    net.Conn
	br      *bufio.Reader
	session *Session

	// Protocol state, guarded by the server lock. While a %begin block is
	// open for this client, notifications and %exit are deferred until
	// after the closing guard: iTerm2 treats every line inside a block as
	// response body.
	inBlock     bool
	pending     []string
	pendingExit *string
	exiting     bool

	mu     sync.Mutex // guards buf/closed; leaf lock (never take server lock under it)
	buf    bytes.Buffer
	cond   *sync.Cond
	closed bool
}

func newClient(s *Server, conn net.Conn, br *bufio.Reader) *Client {
	c := &Client{server: s, conn: conn, br: br}
	c.cond = sync.NewCond(&c.mu)
	go c.writeLoop()
	return c
}

// queue appends raw bytes to the client's outbound buffer.
func (c *Client) queue(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.buf.Len()+len(data) > maxClientBacklog {
		c.server.logger.Printf("client backlog exceeded, dropping connection")
		c.closed = true
		c.conn.Close()
		c.cond.Signal()
		return
	}
	c.buf.Write(data)
	c.cond.Signal()
}

// queueLine appends one protocol line.
func (c *Client) queueLine(line string) {
	c.queue([]byte(line + "\r\n"))
}

// notify delivers a notification line, deferring it while a response block
// is open. Must be called with the server lock held.
func (c *Client) notify(line string) {
	if c.exiting {
		return
	}
	if c.inBlock {
		c.pending = append(c.pending, line)
		return
	}
	c.queueLine(line)
}

func (c *Client) writeLoop() {
	for {
		c.mu.Lock()
		for c.buf.Len() == 0 && !c.closed {
			c.cond.Wait()
		}
		if c.buf.Len() == 0 && c.closed {
			c.mu.Unlock()
			c.conn.Close()
			return
		}
		data := append([]byte(nil), c.buf.Bytes()...)
		c.buf.Reset()
		closing := c.closed
		c.mu.Unlock()

		if _, err := c.conn.Write(data); err != nil {
			c.mu.Lock()
			c.closed = true
			c.mu.Unlock()
			c.conn.Close()
			return
		}
		if closing {
			c.conn.Close()
			return
		}
	}
}

// exit ends the control-mode conversation: %exit [reason], DCS terminator,
// then close once the buffer drains. Deferred while a block is open. Must
// be called with the server lock held.
func (c *Client) exit(reason string) {
	if c.exiting {
		return
	}
	if c.inBlock {
		if c.pendingExit == nil {
			r := reason
			c.pendingExit = &r
		}
		return
	}
	c.exiting = true
	c.session = nil
	line := "%exit"
	if reason != "" {
		line = "%exit " + reason
	}
	c.queue([]byte(line + "\r\n\033\\"))
	c.mu.Lock()
	c.closed = true // writeLoop closes the socket after flushing
	c.cond.Signal()
	c.mu.Unlock()
}

// blockRef identifies an open %begin block.
type blockRef struct {
	ts    int64
	num   int
	flags int
}

// beginBlock emits %begin and opens the block. Server lock held.
func (c *Client) beginBlock(flags int) blockRef {
	b := blockRef{ts: time.Now().Unix(), num: c.server.blockNumLocked(), flags: flags}
	c.queueLine(fmt.Sprintf("%%begin %d %d %d", b.ts, b.num, b.flags))
	c.inBlock = true
	return b
}

// endBlock emits the closing guard, then flushes deferred notifications and
// any deferred %exit. Server lock held.
func (c *Client) endBlock(b blockRef, body string, isError bool) {
	for _, line := range splitLines(body) {
		c.queueLine(line)
	}
	guard := "%end"
	if isError {
		guard = "%error"
	}
	c.queueLine(fmt.Sprintf("%s %d %d %d", guard, b.ts, b.num, b.flags))
	c.inBlock = false
	for _, line := range c.pending {
		c.queueLine(line)
	}
	c.pending = nil
	if c.pendingExit != nil {
		reason := *c.pendingExit
		c.pendingExit = nil
		c.exit(reason)
	}
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// run drives a control-mode client from attach to disconnect.
func (c *Client) run(mode string) {
	s := c.server
	defer func() {
		s.mu.Lock()
		s.removeClientLocked(c)
		s.mu.Unlock()
		c.mu.Lock()
		c.closed = true
		c.cond.Signal()
		c.mu.Unlock()
	}()

	// Control mode preamble. iTerm2 requires the DCS introducer before any
	// protocol line, then an initial server-originated (flags=0) block.
	c.queue([]byte("\033P1000p"))

	s.mu.Lock()
	switch mode {
	case "new":
		sess, err := s.newSessionLocked()
		if err != nil {
			b := c.beginBlock(0)
			c.endBlock(b, err.Error(), true)
			c.exit("")
			s.mu.Unlock()
			return
		}
		c.session = sess
		b := c.beginBlock(0)
		c.endBlock(b, "", false)
		// Match tmux's new-session notification order exactly:
		// %window-add, %sessions-changed, %session-changed.
		c.notify(fmt.Sprintf("%%window-add @%d", sess.active.id))
		s.broadcastLocked(nil, "%%sessions-changed")
		c.notify(fmt.Sprintf("%%session-changed $%d %s", sess.id, sess.name))
	case "attach":
		sess := s.mostRecentSessionLocked()
		if sess == nil {
			b := c.beginBlock(0)
			c.endBlock(b, "no sessions", true)
			c.exit("")
			s.mu.Unlock()
			return
		}
		c.session = sess
		sess.activity = time.Now()
		b := c.beginBlock(0)
		c.endBlock(b, "", false)
		c.notify(fmt.Sprintf("%%session-changed $%d %s", sess.id, sess.name))
	}
	s.mu.Unlock()

	c.readLoop()
}

// readLoop parses command lines from the client. iTerm2 terminates commands
// with '\r' by default (configurable to '\n'), so both are accepted.
func (c *Client) readLoop() {
	var line []byte
	buf := make([]byte, 8192)
	for {
		n, err := c.br.Read(buf)
		for _, ch := range buf[:n] {
			if ch == '\r' || ch == '\n' {
				if len(line) > 0 {
					c.handleLine(string(line))
					line = line[:0]
				}
				continue
			}
			line = append(line, ch)
			if len(line) > 1<<20 {
				// A megabyte without a terminator is not a real command.
				c.server.logger.Printf("oversized command line, dropping client")
				c.conn.Close()
				return
			}
		}
		if err != nil {
			// Connection gone (SSH drop, detach, or iTerm2 quit). The
			// session lives on; just detach this client.
			c.server.mu.Lock()
			c.server.logger.Printf("client disconnected")
			c.server.mu.Unlock()
			return
		}
	}
}

// handleLine executes one command line (possibly a `;`-separated list),
// emitting exactly one %begin/%end|%error block per sub-command, in order.
// After an error, remaining sub-commands are dropped (matching tmux;
// iTerm2's FIFO accounting depends on this).
func (c *Client) handleLine(line string) {
	s := c.server
	s.mu.Lock()
	defer s.mu.Unlock()

	if c.exiting {
		return
	}
	cmds := parseCommandLine(line)
	for _, argv := range cmds {
		if len(argv) == 0 {
			continue
		}
		b := c.beginBlock(1)
		body, err := s.execCommand(c, argv)
		if err != nil {
			c.endBlock(b, err.Error(), true)
			break
		}
		c.endBlock(b, body, false)
		if c.exiting {
			break
		}
	}
}
