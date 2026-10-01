// Package screening holds the Screening List (compliance-screening.md): a static,
// operator-maintained blacklist of addresses, loaded from a YAML file at startup.
// Copy of ledger-service's loader: one file (infra/screening.yaml), mounted identically everywhere.
package screening

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var addressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// List is an immutable set of blacklisted addresses.
type List struct{ set map[string]struct{} }

// Load reads a file of the form `addresses: ["0x...", ...]`. An empty list is
// valid (nothing flagged); a malformed address fails startup, because a typo there
// would silently let a flagged sender through.
func Load(path string) (*List, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading screening list: %w", err)
	}
	var f struct {
		Addresses []string `yaml:"addresses"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parsing screening list: %w", err)
	}
	l := &List{set: make(map[string]struct{}, len(f.Addresses))}
	for i, a := range f.Addresses {
		if !addressRe.MatchString(a) {
			return nil, fmt.Errorf("screening list entry %d %q is not a 0x-prefixed 20-byte hex address", i, a)
		}
		l.set[strings.ToLower(a)] = struct{}{}
	}
	return l, nil
}

// New builds a list directly (tests, or an always-empty default).
func New(addresses ...string) *List {
	l := &List{set: map[string]struct{}{}}
	for _, a := range addresses {
		l.set[strings.ToLower(a)] = struct{}{}
	}
	return l
}

// Contains reports whether the address is on the list (case-insensitive).
func (l *List) Contains(address string) bool {
	_, ok := l.set[strings.ToLower(address)]
	return ok
}

func (l *List) Len() int { return len(l.set) }
