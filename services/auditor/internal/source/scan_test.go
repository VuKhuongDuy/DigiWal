package source

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var anySQL = regexp.MustCompile(`(?i)^\s*(select|with|insert|update|delete|create|alter|drop|truncate)\b`)

// scanLiterals calls fn for every string literal in the auditor's non-test Go code.
func scanLiterals(t *testing.T, fn func(file, lit string)) {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "testdb" {
			return fs.SkipDir // test scaffolding (CREATE DATABASE), never linked into the service
		}
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if s, err := strconv.Unquote(bl.Value); err == nil {
					fn(filepath.ToSlash(path), s)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Invariant 9: no query names a key table. The only file that may spell them is the list that forbids them.
func TestNoQueryTouchesKeyTables(t *testing.T) {
	scanLiterals(t, func(file, lit string) {
		if strings.HasSuffix(file, "internal/source/readonly.go") {
			return
		}
		for _, tbl := range ForbiddenTables {
			if strings.Contains(strings.ToLower(lit), tbl) {
				t.Errorf("%s: string literal mentions %s: %.60q", file, tbl, lit)
			}
		}
	})
}

// Invariant 1: SQL lives in internal/source (foreign stores: SELECT only) or internal/store (the auditor's own database).
func TestSQLIsIsolatedAndReadOnly(t *testing.T) {
	scanLiterals(t, func(file, lit string) {
		if !anySQL.MatchString(lit) || !strings.Contains(lit, " ") {
			return
		}
		switch {
		case strings.Contains(file, "internal/store/"):
			// the auditor's own database: writes allowed
		case strings.Contains(file, "internal/source/"):
			if err := CheckSelect(lit); err != nil {
				t.Errorf("%s: %v", file, err)
			}
		default:
			t.Errorf("%s: SQL outside internal/source and internal/store: %.60q", file, lit)
		}
	})
}
