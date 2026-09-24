package project

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Project source guards that remain useful independently of native Group policy.
//
// Migration inventory is hand-written because it drives the Up/Down residue
// check; keeping it beside the test makes newly added migrations fail closed.

// TestProjectMigrationListCoversEveryMigrationFile pins projectMigrationFiles
// against what the module actually ships.
//
// The list is hand-written and drives TestMigrationUpDownUpLeavesNoResidue, which
// is the only thing that runs a migration's Down section at all. A migration
// added without being listed is therefore never exercised in either direction:
// its Up is applied by the real module loader (so tests pass), its Down is never
// run by anything (so a broken rollback ships), and the residue check silently
// stops covering the new tables.
//
// P2 hit exactly this — the list had to be edited by hand — so the guard is added
// with it rather than after the next one is forgotten.
func TestProjectMigrationListCoversEveryMigrationFile(t *testing.T) {
	entries, err := os.ReadDir("sql")
	require.NoError(t, err)

	onDisk := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		onDisk = append(onDisk, filepath.ToSlash(filepath.Join("sql", e.Name())))
	}
	sort.Strings(onDisk)

	listed := append([]string(nil), projectMigrationFiles...)
	sort.Strings(listed)

	require.Equal(t, onDisk, listed,
		"projectMigrationFiles is out of step with modules/project/sql.\n"+
			"Every migration must be listed, in APPLY order, or its Down section is never "+
			"executed by any test and TestMigrationUpDownUpLeavesNoResidue stops covering "+
			"whatever it creates.")
}
