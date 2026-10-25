//go:build darwin

package seedpage

import "syscall"

// No MADV_DONTDUMP on macOS (development hosts only; the service runs on Linux).
func dontDump([]byte) {}

// Harden sets the core size limit to zero (macOS has no PR_SET_DUMPABLE).
func Harden() error {
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
}
