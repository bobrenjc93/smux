package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Best-effort /proc probes for pane format variables. iTerm2 polls
// pane_current_command and pane_current_path for tab titles and
// new-tab working directories; failures just yield empty strings.

func (p *Pane) pid() int {
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// fgPid returns the pane's foreground process group leader, falling back to
// the shell itself.
func (p *Pane) fgPid() int {
	if p.ptmx != nil {
		if pgid, err := unix.IoctlGetInt(int(p.ptmx.Fd()), unix.TIOCGPGRP); err == nil && pgid > 0 {
			return pgid
		}
	}
	return p.pid()
}

func (p *Pane) currentCommand() string {
	pid := p.fgPid()
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (p *Pane) currentPath() string {
	pid := p.fgPid()
	if pid <= 0 {
		return ""
	}
	path, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return ""
	}
	return path
}

func (p *Pane) tty() string {
	if p.ptmx == nil {
		return ""
	}
	return ptsName(p.ptmx)
}
