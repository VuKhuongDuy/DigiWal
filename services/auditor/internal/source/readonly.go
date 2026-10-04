// Package source is the ONLY place the auditor touches another service's store
// (docs/design/reconciliation-audit.md invariants 1 and 9). Everything here is read-only twice over: the
// database role has no write grants (roles/), and every statement is checked to be a plain SELECT before
// it is sent.
package source

import (
	"fmt"
	"regexp"
	"strings"
)

// ForbiddenTables hold key material. Full read access is a dev convenience, not a reason to read them
// (invariant 9). No role is granted them and no query may name them.
var ForbiddenTables = []string{"wallet_seeds", "master_wallet"}

var (
	leadingNoise = regexp.MustCompile(`^(\s+|--[^\n]*\n|/\*.*?\*/)*`)
	startsRead   = regexp.MustCompile(`(?i)^(select|with)\b`)
	// Any of these anywhere in a statement (including inside a WITH) means it is not a plain read.
	writeWords = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|truncate|drop|alter|create|grant|revoke|copy|call|do|vacuum|lock|set|reset|listen|notify|refresh|comment|import)\b`)
)

// CheckSelect returns an error unless sql is a single read-only SELECT (or WITH ... SELECT) that does not
// name a forbidden table. It is deliberately conservative: a false rejection is a code change, a false
// acceptance is a write to a service's database.
func CheckSelect(sql string) error {
	s := leadingNoise.ReplaceAllString(sql, "")
	s = strings.TrimRight(strings.TrimSpace(s), ";")
	if !startsRead.MatchString(s) {
		return fmt.Errorf("auditor may only SELECT, got %q", head(s))
	}
	if strings.Contains(s, ";") {
		return fmt.Errorf("auditor sends one statement at a time: %q", head(s))
	}
	if m := writeWords.FindString(s); m != "" {
		// FOR UPDATE / FOR SHARE take row locks, which a read-only reader must not do either.
		return fmt.Errorf("auditor statement contains %q: %q", strings.ToUpper(m), head(s))
	}
	if t := forbiddenIn(s); t != "" {
		return fmt.Errorf("auditor must not read %s: %q", t, head(s))
	}
	return nil
}

func forbiddenIn(s string) string {
	l := strings.ToLower(s)
	for _, t := range ForbiddenTables {
		if strings.Contains(l, t) {
			return t
		}
	}
	return ""
}

func head(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}
