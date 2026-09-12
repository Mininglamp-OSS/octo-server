package project

// Why a whole guard for doc comments, in a package where the comments ARE the
// contract.
//
// Round 16's fix inserted a test hook's comment block directly beneath
// ProjectMemberships' doc comment with no blank line between them. Go attaches a
// comment group to whatever declaration follows it, so the ~70 lines carrying the
// read-order invariant, the Space-half rationale and the account-axis rationale
// silently became the documentation of an unexported var, and
// `go doc ./pkg/project ProjectMemberships` printed the signature and nothing else.
// Two reviewers found it independently.
//
// Both of them proposed the same one-line remedy — insert a blank line — and it
// does not work: a blank line only DETACHES the block, leaving it attached to
// nothing while the function is still undocumented. The seam had to move instead.
// That is the reason this is a parser check rather than a "there is a comment near
// the function" grep: the defect is precisely a comment that is present, correct,
// and bound to the wrong declaration, which every textual check reports as fine.
//
// Categorical rather than an enumeration, on purpose. A guard that names
// ProjectMemberships is one more hand-maintained list of the kind this branch has
// already had six findings against; "every exported declaration in this package is
// documented" needs no upkeep and covers the function that has not been written yet.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEveryExportedDeclarationInThisPackageIsDocumented(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, filepath.Clean(name), nil, parser.ParseComments)
		require.NoError(t, parseErr)

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil || !d.Name.IsExported() {
					continue
				}
				checked++
				assert.NotNilf(t, d.Doc, undocumented, name, "func", d.Name.Name)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						checked++
						assert.Truef(t, d.Doc != nil || s.Doc != nil, undocumented, name, "type", s.Name.Name)
					case *ast.ValueSpec:
						for _, id := range s.Names {
							if !id.IsExported() {
								continue
							}
							checked++
							assert.Truef(t, d.Doc != nil || s.Doc != nil, undocumented, name, "var/const", id.Name)
						}
					}
				}
			}
		}
	}

	// A parser guard that parses nothing reports a package with no contract as a
	// package in perfect shape. There were 22 exported declarations when this was
	// written and 15 after main's #887 moved the all-member-group surface out of this
	// package; a materially lower count means the sweep stopped finding them.
	//
	// The floor is re-based rather than left where it was, because a floor that fails
	// for a legitimate deletion teaches people to lower it without reading it — and it
	// is lowered only to just under the real count, so it still catches a parser that
	// stops matching.
	require.GreaterOrEqual(t, checked, 14,
		"the sweep examined %d exported declarations; there were 15 when this floor was "+
			"last re-based", checked)
}

const undocumented = "%s: exported %s %s has no doc comment.\n\n" +
	"In this package the comment IS the contract — the read-order invariant, the " +
	"Space-half conjunction and the account-axis exclusion are all stated nowhere " +
	"else. Check first whether the comment actually exists but has been bound to a " +
	"neighbouring declaration: a comment block with no blank line before a " +
	"declaration becomes THAT declaration's doc comment, which is how " +
	"ProjectMemberships lost its contract to an unexported test hook in round 16. " +
	"Inserting a blank line does not repair that, it only detaches the block; move " +
	"the intruding declaration instead."
