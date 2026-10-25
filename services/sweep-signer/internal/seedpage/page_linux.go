//go:build linux

package seedpage

import "syscall"

// madvDontDump is MADV_DONTDUMP (linux/mman-common.h): the page is left out of core dumps.
const madvDontDump = 16

func dontDump(b []byte) { _ = syscall.Madvise(b, madvDontDump) }

// prDumpable is PR_SET_DUMPABLE (linux/prctl.h).
const prDumpable = 4

// Harden marks the process non-dumpable (no ptrace attach by a same-uid process, no core file) and sets the core size
// limit to zero. Called first thing at start (invariant 16). Errors are returned so the caller can log them; a container
// that forbids prctl is not made weaker by the call failing.
func Harden() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prDumpable, 0, 0); e != 0 {
		return e
	}
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
}
