package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// End-to-end tests of the real smux binary: client + auto-spawned server,
// with the client running on a PTY the way it does under SSH.

var testBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "smux-bin")
	if err == nil {
		bin := filepath.Join(dir, "smux")
		if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err == nil {
			testBin = bin
		} else {
			os.Stderr.WriteString("warning: could not build smux binary; skipping e2e tests: " + string(out) + "\n")
		}
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

type e2eClient struct {
	t    *testing.T
	cmd  *exec.Cmd
	ptmx *os.File
}

func startClient(t *testing.T, tmpdir string, args ...string) *e2eClient {
	t.Helper()
	cmd := exec.Command(testBin, args...)
	cmd.Env = append(os.Environ(), "SMUX_TMPDIR="+tmpdir, "SHELL=/bin/sh")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return &e2eClient{t: t, cmd: cmd, ptmx: ptmx}
}

// readUntil reads from the client's pty until the accumulated output
// contains want, or the deadline passes.
func (c *e2eClient) readUntil(want string, timeout time.Duration) string {
	c.t.Helper()
	var out strings.Builder
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		c.ptmx.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := c.ptmx.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			if strings.Contains(out.String(), want) {
				return out.String()
			}
		}
		if err != nil && !os.IsTimeout(err) {
			break
		}
	}
	c.t.Fatalf("never saw %q in client output; got %q", want, out.String())
	return ""
}

func (c *e2eClient) write(s string) {
	c.t.Helper()
	if _, err := c.ptmx.Write([]byte(s)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func TestE2ELifecycleAndSSHDrop(t *testing.T) {
	if testBin == "" {
		t.Skip("smux binary unavailable")
	}
	tmpdir, err := os.MkdirTemp("/tmp", "smux-e2e")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)
	defer func() {
		cmd := exec.Command(testBin, "kill-server")
		cmd.Env = append(os.Environ(), "SMUX_TMPDIR="+tmpdir)
		cmd.Run()
	}()

	// 1. smux -CC: spawns the server, enters control mode.
	c1 := startClient(t, tmpdir, "-CC")
	c1.readUntil("\033P1000p", 5*time.Second)
	c1.readUntil("%session-changed $0 0", 5*time.Second)

	// 2. Type into the shell through control mode.
	c1.write("send -t %0 -l \"echo SURVIVE-ME; sleep 300 &\"\rsend -H -t %0 0d\r")
	c1.readUntil("SURVIVE-ME", 10*time.Second)

	// 3. Hard-kill the client (SSH connection dropped). The server and
	// session must be unaffected.
	c1.cmd.Process.Kill()
	c1.cmd.Wait()
	c1.ptmx.Close()
	time.Sleep(300 * time.Millisecond)

	// 4. smux -CC a: reattach, session intact, content restorable.
	c2 := startClient(t, tmpdir, "-CC", "a")
	c2.readUntil("%session-changed $0 0", 5*time.Second)
	c2.write("capture-pane -peqJ -t %0 -S -2000\r")
	c2.readUntil("SURVIVE-ME", 10*time.Second)

	// 5. Clean detach.
	c2.write("detach\r")
	c2.readUntil("%exit", 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- c2.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not exit after detach")
	}
	c2.ptmx.Close()

	// 6. Server is still running (session still alive); kill it.
	kill := exec.Command(testBin, "kill-server")
	kill.Env = append(os.Environ(), "SMUX_TMPDIR="+tmpdir)
	if out, err := kill.CombinedOutput(); err != nil {
		t.Fatalf("kill-server: %v %s", err, out)
	}
}

func TestE2EAttachNoServer(t *testing.T) {
	if testBin == "" {
		t.Skip("smux binary unavailable")
	}
	tmpdir, err := os.MkdirTemp("/tmp", "smux-e2e")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	cmd := exec.Command(testBin, "-CC", "a")
	cmd.Env = append(os.Environ(), "SMUX_TMPDIR="+tmpdir)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "no server running") {
		t.Errorf("expected friendly error, got %q", out)
	}
}
