// Command crossmap connects an iPhone plugged into one Mac to the tools
// running on another, across networks.
//
// Run `crossmap agent` on the Mac the iPhone is plugged into, and
// `crossmap host` on the Mac that wants to use it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Version is overridden at build time with -ldflags "-X main.Version=...".
var version = "dev"

const usage = `crossmap - use an iPhone that is plugged into another Mac

usage:
  crossmap init [--psk-file PATH]          create the shared key
  crossmap join KEY [--psk-file PATH]      store a key created on the other Mac
  crossmap agent  [flags]                  run on the Mac the iPhone is plugged into
  crossmap host   [flags]                  run on the Mac that wants the iPhone
  crossmap doctor                          check this machine's setup
  crossmap repair                          restore /var/run/usbmuxd after a crash
  crossmap version

Run "crossmap <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "init":
		err = runInit(args)
	case "join":
		err = runJoin(args)
	case "agent":
		err = runAgent(args)
	case "host":
		err = runHost(args)
	case "doctor":
		err = runDoctor(args)
	case "repair":
		err = runRepair(args)
	case "version", "--version", "-v":
		fmt.Println("crossmap", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "crossmap: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "crossmap:", err)
		os.Exit(1)
	}
}

// newLogger builds the shared structured logger.
func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// signalContext cancels on SIGINT or SIGTERM so daemons can unwind cleanly -
// which, in the host's system mode, means putting Apple's usbmuxd socket back.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// parseAgentRef accepts "name=host:port" or a bare "host:port", in which case
// the host portion becomes the name.
func parseAgentRef(s string) (name, addr string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", errors.New("empty agent reference")
	}
	if name, addr, ok := strings.Cut(s, "="); ok {
		if name == "" || addr == "" {
			return "", "", fmt.Errorf("malformed agent reference %q, want name=host:port", s)
		}
		return name, withDefaultPort(addr), nil
	}
	addr = withDefaultPort(s)
	host := addr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		host = addr[:i]
	}
	return host, addr, nil
}

// DefaultPort is the agent's listening port.
const DefaultPort = "47841"

func withDefaultPort(addr string) string {
	if strings.HasPrefix(addr, "[") {
		if !strings.Contains(addr[strings.LastIndex(addr, "]"):], ":") {
			return addr + ":" + DefaultPort
		}
		return addr
	}
	if strings.Count(addr, ":") == 0 {
		return addr + ":" + DefaultPort
	}
	return addr
}
