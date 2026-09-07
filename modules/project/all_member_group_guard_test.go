package project

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Source guards for the P2 all-member group work.
//
// All three are the tree-walking / list-comparing kind rather than the
// "does this string appear" kind, for the reason P1's D8 gives: a fixed list
// cannot see a new file, and a guard that quietly stops covering things is worse
// than no guard, because it still passes.

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

// TestAllMemberGroupProtectionIsNotInTheServiceLayer pins D7's most important
// constraint: the four refusals live on the HTTP handlers ONLY.
//
// The service-layer primitives they sit in front of — RemoveGroupMembers,
// UpdateMemberRoleTx, UpdateStatusTx — are what P1's project cascade, the Space
// removal cascade, botfather's bot deletion and P2's own owner-sync hook all go
// through. A guard placed there would block the cascades that keep I2 and I4
// true, i.e. the invariant's enforcement would be disabled by the invariant's
// guard.
//
// Checked from THIS module because the constraint is a property of the feature,
// not of modules/group: whoever moves the refusal deeper will be editing
// modules/group and will not think to look here, which is precisely when a guard
// earns its keep.
func TestAllMemberGroupProtectionIsNotInTheServiceLayer(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "group", "service.go"))
	require.NoError(t, err)
	require.NotContains(t, string(body), "refuseIfAllMemberGroup",
		"modules/group/service.go must not call the all-member group guard.\n"+
			"D7's refusals belong on the HTTP handlers. In the service layer they would "+
			"also refuse P1's project cascade, the Space-removal cascade, botfather's bot "+
			"deletion and P2's own owner-sync hook — every path that MAINTAINS the "+
			"invariant this guard exists to protect.")
}
