package main

import (
	"os"

	"golang.org/x/sys/unix"
)

const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)

// ptsName returns the slave path for a pty master. There is no cheap
// portable way to recover it on darwin, and pane_tty is cosmetic (iTerm2
// does not depend on it), so unknown is fine here.
func ptsName(f *os.File) string {
	return ""
}
