// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tests holds repository-wide checks that belong to no single package.
package tests

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const spdx = "SPDX-License-Identifier: AGPL-3.0-or-later"

// Every source file carries the identifier, per ADR-0004. A license that is
// only in LICENSE is a license nobody sees in a diff.
func TestEverySourceFileCarriesTheSPDXIdentifier(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	sourceExt := map[string]bool{".go": true, ".sql": true}
	byName := map[string]bool{"Dockerfile": true, ".env.example": true, "Makefile": true}
	byPrefix := []string{"docker-compose"}
	byExt := map[string]bool{".yml": true}
	skipDir := map[string]bool{".git": true, "data": true, "vendor": true}

	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		named := byName[d.Name()]
		for _, prefix := range byPrefix {
			if strings.HasPrefix(d.Name(), prefix) {
				named = true
			}
		}
		// Workflow files are source too: a license header is cheap and a
		// missing one is the kind of thing nobody notices for a year.
		if byExt[filepath.Ext(d.Name())] && strings.Contains(path, ".github/workflows") {
			named = true
		}
		if !sourceExt[filepath.Ext(d.Name())] && !named {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		checked++

		// The identifier belongs at the top, where a reader and a scanner both
		// find it, so only the first few lines count.
		head := content
		if lines := strings.SplitN(string(content), "\n", 6); len(lines) > 0 {
			head = []byte(strings.Join(lines[:min(len(lines), 5)], "\n"))
		}
		if !strings.Contains(string(head), spdx) {
			t.Errorf("%s has no SPDX identifier in its first lines", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no source files were found, so this check proved nothing")
	}
	t.Logf("checked %d source files", checked)
}
