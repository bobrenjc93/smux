package main

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestDifferentialAgainstTmux runs the same control-mode conversation
// against real tmux (if installed) and against smux, then compares the
// protocol "skeletons": the sequence of block guards, key reply bodies, and
// lifecycle notifications. Timestamps, block numbers, %output, and
// %layout-change noise are normalized away. This pins smux's wire behavior
// to tmux's for the window lifecycle iTerm2 exercises.
func TestDifferentialAgainstTmux(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	if testBin == "" {
		t.Skip("smux binary unavailable")
	}
	t.Setenv("SHELL", "/bin/sh")

	script := []string{
		`list-sessions -F "#{session_id}"`,
		`new-window -PF "#{window_id}"`,
		`list-windows -F "#{window_id} #{?window_active,1,0}"`,
		`select-window -t @0`,
		`kill-window -t @1`,
		`bogus-command-xyz`,
		`send -lt %0 true`,
		`detach`,
	}

	tmuxSkel := runSkeleton(t, tmuxPath,
		[]string{"-CC", "-f", "/dev/null", "-L", fmt.Sprintf("smux-diff-%d", os.Getpid()), "new-session"},
		nil, script)
	smuxDir, err := os.MkdirTemp("/tmp", "smux-diff")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(smuxDir)
	smuxSkel := runSkeleton(t, testBin, []string{"-CC"},
		[]string{"SMUX_TMPDIR=" + smuxDir}, script)

	if !reflect.DeepEqual(tmuxSkel, smuxSkel) {
		t.Errorf("protocol skeletons differ:\ntmux:\n  %s\nsmux:\n  %s",
			strings.Join(tmuxSkel, "\n  "), strings.Join(smuxSkel, "\n  "))
	}
}

func runSkeleton(t *testing.T, bin string, args, extraEnv, script []string) []string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(os.Environ(), "SHELL=/bin/sh"), extraEnv...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	// Collect all output while feeding the script with pacing.
	outc := make(chan byte, 1<<20)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			for _, b := range buf[:n] {
				outc <- b
			}
			if err != nil {
				close(outc)
				return
			}
		}
	}()
	var raw strings.Builder
	drainFor := func(d time.Duration) {
		deadline := time.After(d)
		for {
			select {
			case b, ok := <-outc:
				if !ok {
					return
				}
				raw.WriteByte(b)
			case <-deadline:
				return
			}
		}
	}
	drainFor(1500 * time.Millisecond) // handshake + shell startup
	for _, line := range script {
		ptmx.Write([]byte(line + "\r"))
		drainFor(400 * time.Millisecond)
	}
	drainFor(1 * time.Second)

	return skeletonize(t, raw.String())
}

// skeletonize reduces a control-mode byte stream to comparable events.
func skeletonize(t *testing.T, raw string) []string {
	t.Helper()
	raw = strings.TrimPrefix(raw, "\033P1000p")
	var out []string
	inBlock := false
	blockIdx := 0
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "%begin "):
			inBlock = true
			out = append(out, "BEGIN flags="+line[strings.LastIndex(line, " ")+1:])
		case strings.HasPrefix(line, "%end "):
			inBlock = false
			blockIdx++
			out = append(out, "END")
		case strings.HasPrefix(line, "%error "):
			inBlock = false
			blockIdx++
			out = append(out, "ERROR")
		case inBlock:
			// Keep only deterministic body lines: ids and error text.
			switch {
			case strings.HasPrefix(line, "$") || strings.HasPrefix(line, "@"):
				out = append(out, "  "+line)
			case strings.Contains(line, "unknown command"):
				out = append(out, "  unknown command")
			}
		case strings.HasPrefix(line, "%output "),
			strings.HasPrefix(line, "%layout-change "),
			strings.HasPrefix(line, "%extended-output "):
			// Shell/timing noise: ignore.
		case strings.HasPrefix(line, "%"):
			out = append(out, line)
		}
	}
	return out
}
