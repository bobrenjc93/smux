package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)

// ptsName returns the slave path for a pty master.
func ptsName(f *os.File) string {
	n, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPTN)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("/dev/pts/%d", n)
}
