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
// Both derive the set they check rather than naming it, for the reason P1's D8
// gives: a fixed list cannot see a new file, and a guard that quietly stops
// covering things is worse than no guard, because it still passes.
//
// The second one used to name `../group/service.go` and only that file, which was
// the very rule it was written under. PR #855's second review pointed out that of
// the primitives its own message names, only RemoveGroupMembers is in service.go:
// UpdateMemberRoleTx and UpdateStatusTx are in db.go, and admitOrRestoreMembersTx
// — the one the all-member admitter itself goes through — is in admission.go. A
// refusal moved into any of those kept the guard green while disabling every
// cascade it exists to protect. It globs the package now.

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
	groupDir := filepath.Join("..", "group")
	entries, err := os.ReadDir(groupDir)
	require.NoError(t, err)

	// api.go is where the refusals belong; every other non-test file in the package
	// is a place they must not appear. Naming the ALLOWED file rather than the
	// forbidden ones is what makes this survive a new file being added.
	const handlerFile = "api.go"

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == handlerFile {
			continue
		}
		// all_member_group_guard.go DEFINES the guard; it is not a call site.
		if name == "all_member_group_guard.go" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(groupDir, name))
		require.NoError(t, err)
		scanned++
		require.NotContains(t, string(body), "refuseIfAllMemberGroup(",
			"modules/group/%s must not call the all-member group guard.\n"+
				"D7's refusals belong on the HTTP handlers (%s). Anywhere else in this "+
				"package they would also refuse P1's project cascade, the Space-removal "+
				"cascade, botfather's bot deletion and P2's own owner-sync hook — every "+
				"path that MAINTAINS the invariant this guard exists to protect. The "+
				"primitives are spread across service.go, db.go and admission.go, which is "+
				"why this scans the package rather than one file.", name, handlerFile)
	}
	require.Greater(t, scanned, 10,
		"expected to scan the whole modules/group package, saw %d files — if the scan "+
			"stopped matching, this guard passes vacuously", scanned)
}
