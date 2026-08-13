package main

// Argument handling and output safety for `app cutover`.
//
// Same reasoning as session_rollout_cmd_test.go: this is a surface an operator
// touches by hand during an irreversible procedure. Every case here is
// reachable without MySQL or Redis, because the command rejects bad input —
// and redacts what it prints — before it connects to anything.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	"github.com/stretchr/testify/require"
)

// The dispatcher's documented property: a typo costs a message, not a dial
// against whatever endpoint the config happens to name. These cases pass a
// config path that does not exist, so anything that reaches loadCutoverConfig
// would fail with a "read config" error instead of the expected rejection.
func TestCutoverRejectsUnknownDomainAndActionBeforeConnecting(t *testing.T) {
	const missingConfig = "/nonexistent/cutover-test.yaml"
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no args", nil, "usage: app cutover"},
		{"domain only", []string{"msgextra"}, "usage: app cutover"},
		{"unknown domain", []string{"msgxtra", "preflight", "-config", missingConfig}, `unknown cutover domain "msgxtra"`},
		{"unknown action", []string{"msgextra", "activete", "-config", missingConfig}, `unknown cutover action "activete"`},
		{"unknown action on botevent", []string{"botevent", "flip", "-config", missingConfig}, `unknown cutover action "flip"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runCutoverCommand(tc.args, &out)
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, err.Error(), "read config",
				"validation must happen before the config is read")
			require.Empty(t, out.String(), "a rejected invocation must produce no report")
		})
	}
}

func TestCutoverRejectsPositionalArguments(t *testing.T) {
	var out bytes.Buffer
	err := runCutoverCommand([]string{"msgextra", "preflight", "extra"}, &out)
	require.ErrorContains(t, err, "does not accept positional arguments")
	require.Empty(t, out.String())
}

// Asking for help is not a failure. Without SetOutput(io.Discard) plus the
// ErrHelp branch, `-h` returns flag.ErrHelp, main prints "flag: help requested"
// and exits 1 — and an unknown flag prints its error twice.
func TestCutoverHelpIsNotAnError(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, runCutoverCommand([]string{"msgextra", "preflight", "-h"}, &out))
	require.Contains(t, out.String(), "-floor")
	require.Contains(t, out.String(), "-yes")
}

func TestCutoverUnknownFlagIsReportedOnce(t *testing.T) {
	var out bytes.Buffer
	err := runCutoverCommand([]string{"msgextra", "activate", "-flor", "5"}, &out)
	require.ErrorContains(t, err, "-flor")
	require.Empty(t, out.String(), "the flag package must not also print usage to the report writer")
}

// A domain registered without one of its three actions would panic at dispatch,
// during the procedure it exists to run. docs/cutover-framework.md makes
// registering a domain here a standing instruction, so the wiring is checked
// rather than trusted.
func TestEveryCutoverDomainIsFullyWired(t *testing.T) {
	domains := cutoverDomainList()
	require.NotEmpty(t, domains)
	seen := map[string]bool{}
	for _, d := range domains {
		require.NotEmpty(t, d.name, "domain needs a name")
		require.False(t, seen[d.name], "duplicate domain name %q", d.name)
		seen[d.name] = true
		require.NotEmpty(t, d.summary, "%s: needs a summary for the usage line", d.name)
		require.NotNil(t, d.preflight, "%s: preflight action is nil", d.name)
		require.NotNil(t, d.activate, "%s: activate action is nil", d.name)
		require.NotNil(t, d.status, "%s: status action is nil", d.name)
		require.Contains(t, cutoverUsage(), d.name, "%s: missing from usage output", d.name)
	}
}

// The endpoint print exists so an operator can see which server they are about
// to act on. It must not also hand them the password: this output lands in
// terminal scrollback, `kubectl logs`, and audit captures of the cutover.
func TestRedactedMySQLEndpointNeverLeaksTheCredential(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dsn     string
		want    string
		secrets []string
	}{
		{
			name:    "typical DSN",
			dsn:     "root:sup3r-s3cret@tcp(prod-mysql:3306)/dmwork?charset=utf8mb4&parseTime=true",
			want:    "prod-mysql:3306/dmwork",
			secrets: []string{"sup3r-s3cret"},
		},
		{
			name:    "password containing DSN punctuation",
			dsn:     "svc:p@ss(w)ord:1@tcp(10.0.0.5:3306)/dmwork",
			secrets: []string{"p@ss(w)ord:1"},
		},
		{
			name:    "no credential at all",
			dsn:     "tcp(127.0.0.1:3306)/test",
			want:    "127.0.0.1:3306/test",
			secrets: nil,
		},
		{
			// Falling back to the raw string would reintroduce the leak in
			// exactly the case where the format is unexpected.
			name:    "unparseable",
			dsn:     "@@@not-a-dsn@@@ password=hunter2",
			secrets: []string{"hunter2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactedMySQLEndpoint(tc.dsn)
			if tc.want != "" {
				require.Equal(t, tc.want, got)
			}
			for _, secret := range tc.secrets {
				require.NotContains(t, got, secret, "redacted endpoint leaked the credential")
			}
			require.NotContains(t, got, "@", "a bare host:port/schema carries no userinfo")
		})
	}
}

// The guard readout must describe the process it ran in, not imply it can see
// the fleet's environment — both runbooks document running this off-replica.
func TestCutoverGuardReadoutIsScopedToThisProcess(t *testing.T) {
	spellings := msgextraseq.ExpectedModeSpellings()
	env := msgextraseq.ExpectedModeEnv

	var unset bytes.Buffer
	fprintCutoverGuard(&unset, env, spellings, msgextraModeName)
	require.Contains(t, unset.String(), "this process")
	require.Contains(t, unset.String(), "unset")

	t.Setenv(env, "transactional")
	var armed bytes.Buffer
	fprintCutoverGuard(&armed, env, spellings, msgextraModeName)
	require.Contains(t, armed.String(), "transactional")
	require.Contains(t, armed.String(), "this process")

	t.Setenv(env, "transactionl")
	var malformed bytes.Buffer
	fprintCutoverGuard(&malformed, env, spellings, msgextraModeName)
	require.Contains(t, malformed.String(), "MALFORMED")
	require.Contains(t, malformed.String(), "fails every allocation closed")
}

// The CLI must not carry its own copy of the accepted guard values: a second
// table can call a value malformed that the running allocator accepts.
func TestCutoverGuardSpellingsComeFromTheDomains(t *testing.T) {
	for _, tc := range []struct {
		domain    string
		spellings map[string]int
		modeName  func(int) string
		inactive  int
		active    int
	}{
		{"msgextra", msgextraseq.ExpectedModeSpellings(), msgextraModeName, msgextraseq.ModeLegacy, msgextraseq.ModeTransactional},
		{"botevent", botevent.ExpectedModeSpellings(), boteventModeName, botevent.StateModeLegacy, botevent.StateModeIncr},
	} {
		t.Run(tc.domain, func(t *testing.T) {
			require.Len(t, tc.spellings, 2, "each domain asserts exactly two modes")
			var haveInactive, haveActive bool
			for spelling, mode := range tc.spellings {
				require.NotEmpty(t, strings.TrimSpace(spelling))
				// Every spelling must round-trip through the display helper, so
				// status can never print unknown(N) for a value it accepts.
				require.Equal(t, spelling, tc.modeName(mode))
				switch mode {
				case tc.inactive:
					haveInactive = true
				case tc.active:
					haveActive = true
				default:
					t.Fatalf("spelling %q maps to unknown mode %d", spelling, mode)
				}
			}
			require.True(t, haveInactive && haveActive, "both modes need a spelling")
			require.Equal(t, "unknown(7)", tc.modeName(7))
		})
	}
}
