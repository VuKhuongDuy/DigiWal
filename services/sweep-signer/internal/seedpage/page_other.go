//go:build !linux && !darwin

package seedpage

// Platforms without mmap/mlock here fall back to a plain slice: never locked, still zeroed on Release. The service is
// only built for Linux containers; this exists so the package compiles everywhere.
func newPage() (*Page, error) { return &Page{mem: make([]byte, pageSize())}, nil }

func (p *Page) unmap() {}

// Harden is a no-op on this platform.
func Harden() error { return nil }
