package executor

import (
	"fmt"
	"strings"
	"syscall"
)

// signals are the names the supervisor accepts as stop signals and
// reports in exits.
var signals = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGILL: "SIGILL", syscall.SIGABRT: "SIGABRT", syscall.SIGFPE: "SIGFPE",
	syscall.SIGKILL: "SIGKILL", syscall.SIGSEGV: "SIGSEGV", syscall.SIGPIPE: "SIGPIPE",
	syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM", syscall.SIGUSR1: "SIGUSR1",
	syscall.SIGUSR2: "SIGUSR2", syscall.SIGBUS: "SIGBUS", syscall.SIGTRAP: "SIGTRAP",
}

func signalName(s syscall.Signal) string {
	if name, ok := signals[s]; ok {
		return name
	}
	return fmt.Sprintf("signal %d", int(s))
}

// ParseSignal returns the signal named name ("SIGTERM" or "TERM").
func ParseSignal(name string) (syscall.Signal, bool) {
	want := "SIG" + strings.TrimPrefix(strings.ToUpper(name), "SIG")
	for sig, n := range signals {
		if n == want {
			return sig, true
		}
	}
	return 0, false
}
