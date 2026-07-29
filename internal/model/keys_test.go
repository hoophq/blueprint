package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// keyPrefixes name the two bags. A const with one of these prefixes is a bag
// key and is subject to the collision checks below.
var keyPrefixes = []string{"Attr", "Measure"}

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

// SetObservedMeasure derives a third key space out of the first two: every
// measure key implies an attribute key of measure+AsOfSuffix. Those derived
// names are never declared as consts, so the disjointness check above cannot
// see them — a declared attribute that happened to equal one would be
// overwritten by the next timestamped measure, silently, with an RFC 3339
// string where its own value used to be.
//
// Two ways that can happen, both checked here: an attribute that collides
// with a derived name outright, and any bag key that itself ends in the
// suffix (which would make measure/timestamp ambiguous in either direction).
func TestDerivedAsOfKeysCollideWithNothing(t *testing.T) {
	consts := packageStringConsts(t)
	attrs := withPrefix(consts, "Attr")
	measures := withPrefix(consts, "Measure")
	if len(attrs) == 0 || len(measures) == 0 {
		t.Fatalf("found %d Attr* and %d Measure* consts; expected both to be non-empty", len(attrs), len(measures))
	}

	for measure, measureNames := range measures {
		derived := measure + AsOfSuffix
		if attrNames, ok := attrs[derived]; ok {
			t.Errorf("attribute %q (%v) is the observation-time key SetObservedMeasure derives for measure %q (%v); "+
				"timestamping that measure would overwrite the attribute",
				derived, attrNames, measure, measureNames)
		}
	}

	for _, prefix := range keyPrefixes {
		for key, names := range withPrefix(consts, prefix) {
			if strings.HasSuffix(key, AsOfSuffix) {
				t.Errorf("%s* key %q (%v) ends in %q, which is reserved for the observation times "+
					"SetObservedMeasure derives", prefix, key, names, AsOfSuffix)
			}
		}
	}
}

// Keys are also expected to be distinct within a bag: two consts sharing a
// value mean one of them is dead, and whichever scanner uses the loser writes
// into a key another scanner already owns.
func TestKeyConstantsAreUnique(t *testing.T) {
	consts := packageStringConsts(t)
	for _, prefix := range keyPrefixes {
		for key, names := range withPrefix(consts, prefix) {
			if len(names) > 1 {
				t.Errorf("%s* key %q is declared by %v", prefix, key, names)
			}
		}
	}
}

// The collision guard is only as good as its ability to read a declaration, and
// Go accepts several forms for a string constant beyond a bare literal. Reading
// only literals meant a key added as an alias, a concatenation, or an inherited
// value was invisible to the guard, which would then pass — the worst outcome
// for a check whose whole job is to fail.
//
// So this pins both halves of the contract: every form the evaluator claims to
// read, and the promise that a form it cannot read is reported rather than
// skipped. The fixture is synthetic source so the forms can be exercised
// without planting decoys in the real package.
func TestConstEvaluatorReadsEveryDeclarationForm(t *testing.T) {
	const src = `package model

const prefix = "aws_"

const (
	AttrDirect = "direct"
	AttrInherited
)

const (
	AttrAlias     = AttrDirect
	AttrConcat    = prefix + "concat"
	AttrParen     = ("paren")
	AttrChained   = prefix + AttrDirect
	AttrConverted = string(rune(65))

	MeasureNotAString = 3
)
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	values, unevaluated := evalStringConsts([]*ast.File{file})

	want := map[string]string{
		"prefix":        "aws_",
		"AttrDirect":    "direct",
		"AttrInherited": "direct", // a spec with no value repeats the previous one
		"AttrAlias":     "direct",
		"AttrConcat":    "aws_concat",
		"AttrParen":     "paren",
		"AttrChained":   "aws_direct",
	}
	for name, wantValue := range want {
		if got, ok := values[name]; !ok || got != wantValue {
			t.Errorf("%s = %q (present=%v), want %q", name, got, ok, wantValue)
		}
	}

	// A constant conversion and a non-string constant are both beyond this
	// evaluator. Neither may be silently dropped: the caller turns an
	// unevaluated bag key into a failure.
	for _, name := range []string{"AttrConverted", "MeasureNotAString"} {
		if !slices.Contains(unevaluated, name) {
			t.Errorf("%s should be reported as unevaluated, got %v", name, unevaluated)
		}
		if _, ok := values[name]; ok {
			t.Errorf("%s should not have a value, got %q", name, values[name])
		}
	}

	// The point of reading those forms: three consts collide on "direct", and
	// only one of them is a literal. Before this, the guard saw one.
	if names := withPrefix(values, "Attr")["direct"]; len(names) != 3 {
		t.Errorf(`consts colliding on "direct" = %v, want all three of AttrDirect, AttrInherited, AttrAlias`, names)
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

// packageStringConsts returns const-name→string-value for every string const
// declared in the model package. Test files are skipped so a fixture constant
// cannot mask or manufacture a collision.
//
// A bag key whose value cannot be evaluated fails the test rather than being
// dropped from the map: silently skipping it is exactly how a collision would
// slip past the checks above.
func packageStringConsts(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read model package dir: %v", err)
	}
	var files []*ast.File
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
		files = append(files, file)
	}

	values, unevaluated := evalStringConsts(files)
	for _, name := range unevaluated {
		if isKeyConst(name) {
			t.Errorf("%s is named like a bag key but its value could not be evaluated, so the "+
				"collision checks cannot see it; declare it as a string constant built from "+
				"literals and other constants in this package", name)
		}
	}
	return values
}

func isKeyConst(name string) bool {
	for _, p := range keyPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// evalStringConsts returns const-name→string-value for the top-level constants
// declared across files, plus the sorted names of the ones it could not reduce
// to a string. Evaluation spans all files at once because a constant may be an
// alias for one declared in another file of the same package.
func evalStringConsts(files []*ast.File) (map[string]string, []string) {
	exprs := constExprs(files)
	values := make(map[string]string, len(exprs))
	var unevaluated []string
	for name, expr := range exprs {
		if v, ok := evalStringExpr(expr, exprs, 0); ok {
			values[name] = v
		} else {
			unevaluated = append(unevaluated, name)
		}
	}
	sort.Strings(unevaluated)
	return values, unevaluated
}

// constExprs collects the initializing expression of every top-level constant.
// A spec with no values repeats the previous spec's expressions, which is a
// legal way to declare two constants with the same value — and therefore a way
// to collide.
func constExprs(files []*ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			var carried []ast.Expr
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				values := vs.Values
				if len(values) == 0 {
					values = carried
				} else {
					carried = values
				}
				for i, name := range vs.Names {
					if i < len(values) {
						out[name.Name] = values[i]
					}
				}
			}
		}
	}
	return out
}

// maxConstDepth bounds alias chasing. Go rejects constant cycles at compile
// time so real source cannot loop, but the evaluator also runs on test fixtures
// and must not hang on one.
const maxConstDepth = 64

// evalStringExpr reduces a constant expression to its string value, covering
// every form a key in this package could plausibly take: a literal, a reference
// to another constant, a concatenation, and parentheses. Anything else — a
// conversion, an import-qualified constant, a non-string constant — reports
// false so the caller can flag it, rather than guessing a value.
func evalStringExpr(e ast.Expr, exprs map[string]ast.Expr, depth int) (string, bool) {
	if depth > maxConstDepth {
		return "", false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(x.Value)
		return v, err == nil
	case *ast.ParenExpr:
		return evalStringExpr(x.X, exprs, depth+1)
	case *ast.Ident:
		ref, ok := exprs[x.Name]
		if !ok {
			return "", false
		}
		return evalStringExpr(ref, exprs, depth+1)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		left, okLeft := evalStringExpr(x.X, exprs, depth+1)
		right, okRight := evalStringExpr(x.Y, exprs, depth+1)
		if !okLeft || !okRight {
			return "", false
		}
		return left + right, true
	}
	return "", false
}
