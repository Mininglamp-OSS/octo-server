package main

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/stretchr/testify/require"
)

func TestParseAdminCommandRequiresExplicitSafeMigrationParameters(t *testing.T) {
	now := time.Date(2026, time.August, 9, 16, 0, 0, 0, time.UTC)
	args := []string{
		"migrate",
		"--config", "configs/prod.yaml",
		"--campaign", "legacy-2026-08",
		"--cutoff", "2026-08-16T16:00:00Z",
		"--finite-policy", "natural",
		"--batch-size", "200",
		"--qps", "50",
		"--lease", "30s",
	}

	command, err := parseAdminCommand(args, func() time.Time { return now })
	require.NoError(t, err)
	require.Equal(t, adminCommandMigrate, command.kind)
	require.Equal(t, "configs/prod.yaml", command.configPath)
	require.False(t, command.migration.Apply)
	require.Equal(t, "legacy-2026-08", command.migration.CampaignID)
	require.Equal(t, time.Date(2026, time.August, 16, 16, 0, 0, 0, time.UTC), command.migration.CutoffAt)
	require.Equal(t, auth.LegacyFinitePolicyNatural, command.migration.FinitePolicy)
	require.Equal(t, int64(200), command.migration.BatchSize)
	require.Equal(t, 20*time.Millisecond, command.migration.Interval)
	require.Equal(t, 30*time.Second, command.migration.Lease)

	args = append(args, "--apply")
	command, err = parseAdminCommand(args, func() time.Time { return now })
	require.NoError(t, err)
	require.True(t, command.migration.Apply)
}

func TestParseAdminCommandRejectsUnsafeOrAmbiguousMigrationInput(t *testing.T) {
	now := time.Date(2026, time.August, 9, 16, 0, 0, 0, time.UTC)
	valid := []string{
		"migrate",
		"--config", "configs/prod.yaml",
		"--campaign", "legacy-2026-08",
		"--cutoff", "2026-08-16T16:00:00Z",
		"--finite-policy", "natural",
		"--batch-size", "200",
		"--qps", "50",
		"--lease", "30s",
	}
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing campaign", args: withoutFlag(valid, "--campaign")},
		{name: "missing cutoff", args: withoutFlag(valid, "--cutoff")},
		{name: "missing finite policy", args: withoutFlag(valid, "--finite-policy")},
		{name: "unknown finite policy", args: replaceFlag(valid, "--finite-policy", "guess")},
		{name: "missing batch", args: withoutFlag(valid, "--batch-size")},
		{name: "missing qps", args: withoutFlag(valid, "--qps")},
		{name: "missing lease", args: withoutFlag(valid, "--lease")},
		{name: "malformed cutoff", args: replaceFlag(valid, "--cutoff", "next-week")},
		{name: "zero batch", args: replaceFlag(valid, "--batch-size", "0")},
		{name: "oversized batch", args: replaceFlag(valid, "--batch-size", "10001")},
		{name: "zero qps", args: replaceFlag(valid, "--qps", "0")},
		{name: "oversized qps", args: replaceFlag(valid, "--qps", "10001")},
		{name: "short lease", args: replaceFlag(valid, "--lease", "4s")},
		{name: "long lease", args: replaceFlag(valid, "--lease", "6m")},
		{name: "unexpected positional", args: append(append([]string{}, valid...), "extra")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseAdminCommand(tt.args, func() time.Time { return now })
			require.Error(t, err)
		})
	}

	pastApply := append(replaceFlag(valid, "--cutoff", "2026-08-09T15:59:59Z"), "--apply")
	_, err := parseAdminCommand(pastApply, func() time.Time { return now })
	require.ErrorContains(t, err, "--confirm-elapsed-cutoff")

	confirmedPastApply := append(pastApply, "--confirm-elapsed-cutoff")
	command, err := parseAdminCommand(confirmedPastApply, func() time.Time { return now })
	require.NoError(t, err, "an elapsed resumable campaign must require explicit destructive confirmation")
	require.True(t, command.migration.Apply)
	require.True(t, command.migration.ConfirmElapsedCutoff)
}

func TestParseAdminCommandAcceptsOnlyOneStepRolloutTargets(t *testing.T) {
	for _, target := range []auth.SessionMode{
		auth.SessionModeV3Write,
		auth.SessionModeRevoke,
		auth.SessionModeBounded,
		auth.SessionModeEnforce,
	} {
		command, err := parseAdminCommand([]string{
			"advance-floor", "--config", "configs/prod.yaml", "--to", string(target),
			"--observation-min-gap", "1h",
		}, time.Now)
		require.NoError(t, err)
		require.Equal(t, adminCommandAdvanceFloor, command.kind)
		require.Equal(t, target, command.floor)
		require.Equal(t, time.Hour, command.observationMinGap)
	}

	for _, target := range []string{"", "expand", "V3-WRITE", "unknown"} {
		_, err := parseAdminCommand([]string{
			"advance-floor", "--config", "configs/prod.yaml", "--to", target,
			"--observation-min-gap", "1h",
		}, time.Now)
		require.Error(t, err)
	}

	valid := []string{
		"advance-floor", "--config", "configs/prod.yaml", "--to", "v3-write",
		"--observation-min-gap", "1h",
	}
	for _, args := range [][]string{
		withoutFlag(valid, "--observation-min-gap"),
		replaceFlag(valid, "--observation-min-gap", "0s"),
		replaceFlag(valid, "--observation-min-gap", "-1s"),
		replaceFlag(valid, "--observation-min-gap", "59m59s"),
	} {
		_, err := parseAdminCommand(args, time.Now)
		require.Error(t, err)
	}
}

func TestParseAdminCommandRejectsUnknownOrMissingSubcommand(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"unknown"}} {
		_, err := parseAdminCommand(args, time.Now)
		require.Error(t, err)
	}
}

func withoutFlag(args []string, name string) []string {
	result := make([]string, 0, len(args)-2)
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			i++
			continue
		}
		result = append(result, args[i])
	}
	return result
}

func replaceFlag(args []string, name, value string) []string {
	result := append([]string(nil), args...)
	for i := 0; i+1 < len(result); i++ {
		if result[i] == name {
			result[i+1] = value
			return result
		}
	}
	return result
}
