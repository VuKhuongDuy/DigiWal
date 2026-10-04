// Package guard is the auditor's dev-only guard (docs/design/reconciliation-audit.md, Scope and
// invariant 8): this tool has read access to every store, so it must not start against anything but a
// development chain.
package guard

import (
	"fmt"
	"strconv"
	"strings"
)

// Check returns an error unless env is exactly "dev" and chainID is on the allow-list.
func Check(env string, chainID int64, allowed []int64) error {
	if env != "dev" {
		return fmt.Errorf("refusing to start: AUDITOR_ENV=%q, the auditor is a dev-environment tool and needs AUDITOR_ENV=dev", env)
	}
	for _, id := range allowed {
		if id == chainID {
			return nil
		}
	}
	return fmt.Errorf("refusing to start: chain id %d is not on the dev allow-list %v", chainID, allowed)
}

// ParseChainIDs parses a comma-separated list such as "31337,11155111". An empty list is an error: the
// allow-list is what makes the guard mean something.
func ParseChainIDs(s string) ([]int64, error) {
	var ids []int64
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid chain id %q", p)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("the dev chain allow-list is empty")
	}
	return ids, nil
}
