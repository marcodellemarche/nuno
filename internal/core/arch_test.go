// SPDX-License-Identifier: AGPL-3.0-or-later

package core_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// NFR-15: internal/core must not import any other internal package. The rule
// is only meaningful if something checks it, and this is the check. Test files
// are exempt: they are not part of what core compiles into.
func TestCoreImportsNoOtherInternalPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checked++
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if strings.Contains(path, "/internal/") {
				t.Errorf("%s imports %s: core holds the model and the ports, and does no I/O", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no source files found in %s, so this check proved nothing", must(filepath.Abs(".")))
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
