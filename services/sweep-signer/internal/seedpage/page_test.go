package seedpage

import "testing"

func TestPage_WriteReleaseZeroesTheWholeMappingBeforeUnmapping(t *testing.T) {
	p, err := New(false)
	if err != nil {
		t.Fatal(err)
	}
	buf := p.Buffer()
	if len(buf) != Size {
		t.Fatalf("buffer is %d bytes, want %d", len(buf), Size)
	}
	for i := range buf {
		buf[i] = 0xAA
	}
	// the rest of the page too: whatever was written anywhere in it must be gone
	p.mem[len(p.mem)-1] = 0xBB
	p.Release()
	if !p.ZeroedOnRelease() {
		t.Fatal("the mapping was not all zero when it was unmapped")
	}
	if p.Buffer() != nil {
		t.Fatal("Buffer() still hands out memory after Release")
	}
	p.Release() // idempotent, must not panic or unmap twice
	t.Logf("page locked=%v, zeroed before munmap=%v", p.Locked(), p.ZeroedOnRelease())
}

func TestPage_StrictRefusesAnUnlockablePage(t *testing.T) {
	p, err := New(true)
	if err != nil {
		if err != ErrNotLocked {
			t.Fatalf("unexpected error %v", err)
		}
		return // mlock is not permitted here: strict mode refused, as designed
	}
	defer p.Release()
	if !p.Locked() {
		t.Fatal("strict New returned an unlocked page")
	}
}

func TestPage_EachNewPageIsFreshAndZero(t *testing.T) {
	a, _ := New(false)
	for i := range a.Buffer() {
		a.Buffer()[i] = 1
	}
	a.Release()
	b, _ := New(false)
	defer b.Release()
	for i, c := range b.Buffer() {
		if c != 0 {
			t.Fatalf("byte %d of a new page is %d", i, c)
		}
	}
}
