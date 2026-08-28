package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// hello is the one-line JSON handshake a client sends on connect.
type hello struct {
	// Cmd is "new" (create session + attach), "attach" (attach most
	// recent session), or "oneshot" (run Line, print reply, exit).
	Cmd  string `json:"cmd"`
	Line string `json:"line,omitempty"`
}

func socketPath(name string) string {
	dir := os.Getenv("SMUX_TMPDIR")
	if dir == "" {
		dir = fmt.Sprintf("%s/smux-%d", tmpDir(), os.Getuid())
	}
	return filepath.Join(dir, name)
}

func tmpDir() string {
	if d := os.Getenv("TMPDIR"); d != "" {
		return filepath.Clean(d)
	}
	return "/tmp"
}

// connectOrSpawn connects to the server socket, starting the server first if
// it is not running.
func connectOrSpawn(sock string, spawn bool) (net.Conn, error) {
	if c, err := net.Dial("unix", sock); err == nil {
		return c, nil
	}
	if !spawn {
		return nil, fmt.Errorf("no server running on %s", sock)
	}
	// A stale socket (server died without cleanup) must be removed before
	// the new server can bind.
	os.Remove(sock)

	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "-L", filepath.Base(sock), "--server")
	cmd.Env = append(os.Environ(), "SMUX_TMPDIR="+filepath.Dir(sock))
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting server: %w", err)
	}
	// The server is reparented to init via Setsid; Release lets us forget it.
	cmd.Process.Release()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			return c, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("server did not start listening on %s", sock)
}

// runControlClient attaches this terminal to the server in control mode.
// It is a dumb pipe: stdin bytes go to the server, server bytes go to
// stdout. All protocol handling happens server-side.
func runControlClient(sock string, attach bool) int {
	conn, err := connectOrSpawn(sock, !attach)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smux: %v\n", err)
		return 1
	}
	defer conn.Close()

	h := hello{Cmd: "new"}
	if attach {
		h.Cmd = "attach"
	}
	hb, _ := json.Marshal(h)
	if _, err := conn.Write(append(hb, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "smux: %v\n", err)
		return 1
	}

	// Raw mode: the control protocol must not be echoed or line-buffered,
	// and output must pass through untranslated.
	oldState, rawErr := makeRaw(int(os.Stdin.Fd()))
	restore := func() {
		if rawErr == nil {
			unix.IoctlSetTermios(int(os.Stdin.Fd()), ioctlWriteTermios, oldState)
		}
	}
	defer restore()

	// SIGHUP/SIGTERM (e.g. SSH connection dropped): exit cleanly; the
	// server notices the socket close and detaches us. The session lives on.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	errc := make(chan error, 2)
	go func() { // server -> stdout
		_, err := io.Copy(os.Stdout, conn)
		errc <- err
	}()
	go func() { // stdin -> server
		_, err := io.Copy(conn, os.Stdin)
		errc <- err
	}()

	select {
	case <-sigc:
	case <-errc:
	}
	restore()
	return 0
}

// runOneShot sends a single command line and prints the server's plain-text
// reply (used for kill-server / ls).
func runOneShot(sock string, line string) int {
	conn, err := connectOrSpawn(sock, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smux: %v\n", err)
		return 1
	}
	defer conn.Close()
	hb, _ := json.Marshal(hello{Cmd: "oneshot", Line: line})
	if _, err := conn.Write(append(hb, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "smux: %v\n", err)
		return 1
	}
	io.Copy(os.Stdout, conn)
	return 0
}

func makeRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &raw); err != nil {
		return nil, err
	}
	return old, nil
}
