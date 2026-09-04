// Command smux is a minimal terminal multiplexer that speaks the tmux
// control-mode protocol (tmux -CC), targeting iTerm2's tmux integration.
//
// Scope is intentionally narrow: sessions that survive disconnects, windows
// (one pane each), and the control-mode wire protocol iTerm2 needs. It is not
// a general tmux replacement.
package main

import (
	"fmt"
	"os"
)

// version is stamped by the release build via
// -ldflags "-X main.version=v1.2.3".
var version = "dev"

func init() { vtVersion = version }

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  smux -CC            start the session and attach in control mode
                      (errors if a session already exists: attach instead)
  smux -CC a[ttach]   attach to the existing session in control mode
  smux --server       run the server (normally started automatically)
  smux kill-server    terminate the server and the session
  smux ls             show the session

Options:
  -L <name>   use a distinct socket name (default "default")
  -V          print the smux version
`)
	os.Exit(1)
}

func main() {
	args := os.Args[1:]
	socketName := "default"
	controlMode := false

	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-CC":
			controlMode = true
		case "-L":
			if i+1 >= len(args) {
				usage()
			}
			i++
			socketName = args[i]
		case "--server":
			runServer(socketPath(socketName))
			return
		case "-V", "--version":
			fmt.Printf("smux %s\n", version)
			return
		case "-h", "--help":
			usage()
		default:
			rest = append(rest, args[i])
		}
	}

	sock := socketPath(socketName)

	if controlMode {
		// Refuse to start control mode inside an existing tmux or smux
		// pane (mirroring tmux's nesting guard). The outer multiplexer
		// would swallow our control stream as an unterminated DCS string
		// and the front-end (iTerm2/dispatcher) would never see it.
		for _, env := range []string{"TMUX", "SMUX"} {
			if os.Getenv(env) != "" {
				fmt.Fprintf(os.Stderr, "smux: sessions should be nested with care; "+
					"this shell is already inside a %s session (unset $%s to force).\n"+
					"Run smux -CC from a plain terminal, e.g. a fresh ssh connection.\n",
					map[string]string{"TMUX": "tmux", "SMUX": "smux"}[env], env)
				os.Exit(1)
			}
		}
		attach := false
		if len(rest) > 0 {
			switch rest[0] {
			case "a", "at", "att", "atta", "attac", "attach", "attach-session":
				attach = true
			case "new", "new-session":
				attach = false
			default:
				usage()
			}
		}
		os.Exit(runControlClient(sock, attach))
	}

	// One-shot commands (no control mode).
	if len(rest) == 1 {
		switch rest[0] {
		case "kill-server", "ls", "list-sessions":
			os.Exit(runOneShot(sock, rest[0]))
		}
	}
	usage()
}
