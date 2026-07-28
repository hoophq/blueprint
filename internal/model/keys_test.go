package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Attributes and Measures share one flat key space, and consumers rely on it.
// The CSV writer merges both bags into a single cell and the diff walks them as
// one sorted key union; a key declared in both would silently overwrite the
// other in CSV and emit two same-named drift lines. Both of those places
// document the key sets as disjoint "by construction" — this is what makes that
// true, instead of a comment hoping nobody breaks it.
//
// The check reads the package's own const declarations rather than a
// hand-maintained list, because a list is exactly the thing that goes stale the
// day someone adds a service and forgets it — and adding keys is how this model
// is meant to grow.
func TestAttributeAndMeasureKeysAreDisjoint(t *testing.T) {
	consts := packageStringConsts(t)
	attrs := withPrefix(consts, "Attr")
	measures := withPrefix(consts, "Measure")

	// Without this the test would pass vacuously if the parse ever stopped
	// finding declarations.
	if len(attrs) == 0 || len(measures) == 0 {
		t.Fatalf("found %d Attr* and %d Measure* consts; expected both to be non-empty", len(attrs), len(measures))
	}

	for key, attrNames := range attrs {
		if measureNames, ok := measures[key]; ok {
			t.Errorf("key %q is declared as both %v and %v: the bags must not overlap, "+
				"because CSV flattens them into one cell and the diff walks them as one key space",
				key, attrNames, measureNames)
		}
	}
}

// Keys are also expected to be distinct within a bag: two consts sharing a
// value mean one of them is dead, and whichever scanner uses the loser writes
// into a key another scanner already owns.
func TestKeyConstantsAreUnique(t *testing.T) {
	consts := packageStringConsts(t)
	for _, prefix := range []string{"Attr", "Measure"} {
		for key, names := range withPrefix(consts, prefix) {
			if len(names) > 1 {
				t.Errorf("%s* key %q is declared by %v", prefix, key, names)
			}
		}
	}
}

// withPrefix inverts name→value into value→names for the consts whose name
// starts with prefix.
func withPrefix(consts map[string]string, prefix string) map[string][]string {
	out := map[string][]string{}
	for name, value := range consts {
		if strings.HasPrefix(name, prefix) {
			out[value] = append(out[value], name)
		}
	}
	return out
}

// packageStringConsts returns const-name→string-value for every string-literal
// const declared in the model package. Test files are skipped so a fixture
// constant cannot mask or manufacture a collision.
func packageStringConsts(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read model package dir: %v", err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		maps.Copy(out, stringConsts(file))
	}
	return out
}

// stringConsts returns const-name→string-value for every string-literal const
// declared at the top level of file.
func stringConsts(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if v, err := strconv.Unquote(lit.Value); err == nil {
					out[name.Name] = v
				}
			}
		}
	}
	return out
}
