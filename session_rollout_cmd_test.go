package main

// Argument handling for `app session-rollout`.
//
// The command had no test file at all, which mattered more than the line count
// suggests: it is the only surface an operator touches during an incident, its
// flags decide how hard an unattended scan hits the shared session pool, and one
// of them is a break-glass path that writes an irreversible floor. Every case
// here is reachable without Redis or MySQL, because the command now rejects bad
// input before it builds a connection to anything.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionRolloutRejectsUnknownSubcommandsBeforeConnecting(t *testing.T) {
	var out bytes.Buffer
	err := runSessionRolloutCommand([]string{"stats"}, &out)
	require.ErrorContains(t, err, `unknown session-rollout subcommand "stats"`)
	require.Empty(t, out.String())

	err = runSessionRolloutCommand(nil, &out)
	require.ErrorContains(t, err, "usage: app session-rollout")
}

// Every subcommand that can scan must honour the same throttle bounds. status
// used to skip the check entirely, so `status --qps 0` ran an unthrottled full
// keyspace scan while observe and migrate refused the identical argument.
func TestSessionRolloutValidatesScanRateForEveryScanningSubcommand(t *testing.T) {
	for _, sub := range []string{"status", "observe", "migrate", "advance"} {
		t.Run(sub, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				args []string
				want string
			}{
				{"qps zero", []string{"--qps", "0"}, "--qps"},
				{"qps negative", []string{"--qps", "-1"}, "--qps"},
				{"qps above ceiling", []string{"--qps", "10001"}, "--qps"},
				{"batch size zero", []string{"--batch-size", "0"}, "--batch-size"},
				{"batch size negative", []string{"--batch-size", "-5"}, "--batch-size"},
				{"batch size above ceiling", []string{"--batch-size", "10001"}, "--batch-size"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var out bytes.Buffer
					err := runSessionRolloutCommand(append([]string{sub}, tc.args...), &out)
					require.ErrorContains(t, err, tc.want)
					require.Empty(t, out.String(), "a rejected argument must produce no report")
				})
			}
		})
	}
}

func TestSessionRolloutRejectsPositionalArguments(t *testing.T) {
	var out bytes.Buffer
	err := runSessionRolloutCommand([]string{"observe", "extra"}, &out)
	require.ErrorContains(t, err, "does not accept positional arguments")
}

// --force is a bypass of the RECONCILER, never of the predicate, and it takes
// two flags rather than one so it cannot be reached by a single typo.
func TestSessionRolloutAdvanceRequiresBothForceAndYes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		force bool
		yes   bool
	}{
		{"neither", false, false},
		{"force only", true, false},
		{"yes only", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			// A nil store and nil markers are safe: refusal happens before
			// either is touched, which is the property being pinned.
			err := sessionRolloutAdvance(ctx, nil, nil, &out, sessionRolloutAdvanceArgs{
				force: tc.force, yes: tc.yes, batchSize: 200, scanInterval: 10 * time.Millisecond,
			})
			require.ErrorContains(t, err, "advance is the reconciler's job")
			require.Empty(t, out.String())
		})
	}
}

// migrate carries the one decision nothing can make automatically — how many
// people get logged out early — so each input that shapes it is required
// explicitly rather than defaulted.
func TestSessionRolloutMigrateValidatesItsArgumentsBeforeTouchingRedis(t *testing.T) {
	ctx := context.Background()
	valid := sessionRolloutMigrateArgs{
		campaign:     "c1",
		cutoffRaw:    time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		finitePolicy: "natural",
		batchSize:    200,
		scanInterval: 10 * time.Millisecond,
		lease:        30 * time.Second,
	}
	for _, tc := range []struct {
		name   string
		mutate func(a *sessionRolloutMigrateArgs)
		want   string
	}{
		{"no campaign", func(a *sessionRolloutMigrateArgs) { a.campaign = "  " }, "requires --campaign"},
		{"no cutoff", func(a *sessionRolloutMigrateArgs) { a.cutoffRaw = "" }, "requires --cutoff"},
		{"unparseable cutoff", func(a *sessionRolloutMigrateArgs) { a.cutoffRaw = "tomorrow" }, "invalid --cutoff"},
		{"no policy", func(a *sessionRolloutMigrateArgs) { a.finitePolicy = "" }, "must be natural or cap"},
		{"unknown policy", func(a *sessionRolloutMigrateArgs) { a.finitePolicy = "aggressive" }, "must be natural or cap"},
		{
			// The dangerous combination: an apply whose cutoff is already in the
			// past deletes on sight rather than shortening.
			name: "elapsed cutoff with apply and no confirmation",
			mutate: func(a *sessionRolloutMigrateArgs) {
				a.apply = true
				a.cutoffRaw = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
			},
			want: "--confirm-elapsed-cutoff",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := valid
			tc.mutate(&args)
			var out bytes.Buffer
			err := sessionRolloutMigrate(ctx, nil, &out, args)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, out.String())
		})
	}
}

func TestValidateScanRateAcceptsTheDocumentedRange(t *testing.T) {
	require.NoError(t, validateScanRate(100, 200))
	require.NoError(t, validateScanRate(0.5, 1), "a deliberately slow scan is legitimate")
	require.NoError(t, validateScanRate(10_000, 10_000))
}

// The help text is the only place an operator learns these bounds, so it has to
// name the ones the code enforces.
func TestSessionRolloutHelpTextMatchesTheEnforcedBounds(t *testing.T) {
	require.ErrorContains(t, validateScanRate(100, 10_001), "1 and 10000")
	require.ErrorContains(t, validateScanRate(10_001, 200), "no more than 10000")
	source, err := os.ReadFile("session_rollout_cmd.go")
	require.NoError(t, err)
	require.True(t, strings.Contains(string(source), "Redis SCAN count hint (1-10000)"),
		"the --batch-size help text names the enforced range")
}
