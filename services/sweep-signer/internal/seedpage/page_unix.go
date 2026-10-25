//go:build linux || darwin

package seedpage

import "syscall"

func newPage() (*Page, error) {
	mem, err := syscall.Mmap(-1, 0, pageSize(), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	p := &Page{mem: mem}
	// Best effort, in this order: no core dump of it (Linux), then no swap.
	dontDump(mem)
	if err := syscall.Mlock(mem); err == nil {
		p.locked = true
	}
	return p, nil
}

func (p *Page) unmap() {
	if p.locked {
		_ = syscall.Munlock(p.mem)
	}
	_ = syscall.Munmap(p.mem)
}
