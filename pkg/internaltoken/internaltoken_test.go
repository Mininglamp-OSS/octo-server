package internaltoken

import (
	"errors"
	"strings"
	"testing"
)

// longEnough clears DefaultMinBytes so length never masks the case under test.
func longEnough(seed string) string {
	return strings.Repeat(seed, DefaultMinBytes)[:DefaultMinBytes]
}

// envMap builds a getenv over an explicit map so a test can never accidentally
// read the ambient process environment.
func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// TestResolveCoversEveryRegisteredPair is the point of the package: every
// unordered pair of registered envs is checked, exactly once, by whichever of
// the two was registered later. A hand-rolled per-module check only ever covered
// the siblings its author happened to know about; this enumerates the registry,
// so appending an env covers it against all the existing ones with no test edit.
//
// It also pins the precedence rule that decides which side yields: the junior
// env is disabled and the senior one keeps serving. Disabling both would take a
// working ingress offline for a misconfiguration that is already contained once
// either side is off.
func TestResolveCoversEveryRegisteredPair(t *testing.T) {
	envs := Envs()
	if len(envs) < 2 {
		t.Fatalf("registry has %d envs; the pairwise invariant needs at least 2", len(envs))
	}
	shared := longEnough("s")

	pairs := 0
	for juniorIndex, junior := range envs {
		for _, senior := range envs[:juniorIndex] {
			pairs++
			t.Run(junior+"_yields_to_"+senior, func(t *testing.T) {
				getenv := envMap(map[string]string{junior: shared, senior: shared})

				token, err := Resolve(junior, getenv)
				if err == nil {
					t.Fatalf("Resolve(%s) = %q, nil; a value shared with the senior env %s must disable it",
						junior, token, senior)
				}
				if token != "" {
					t.Fatalf("Resolve(%s) token = %q; a refused token must be empty so auth fails closed",
						junior, token)
				}
				var resolveErr *Error
				if !errors.As(err, &resolveErr) {
					t.Fatalf("Resolve(%s) error type = %T, want *internaltoken.Error", junior, err)
				}
				if resolveErr.Reason != ReasonCollision {
					t.Fatalf("Resolve(%s) reason = %q, want %q", junior, resolveErr.Reason, ReasonCollision)
				}
				if resolveErr.Other != senior {
					t.Fatalf("Resolve(%s) collided with %q, want %q", junior, resolveErr.Other, senior)
				}
				if !strings.Contains(err.Error(), senior) {
					t.Fatalf("reason %q must name the colliding env %s so an operator can act on it",
						err.Error(), senior)
				}

				// The senior capability keeps serving: it is the incumbent, and
				// the invariant ("one credential, one capability") is satisfied
				// the moment the junior one is off.
				seniorToken, seniorErr := Resolve(senior, getenv)
				if seniorErr != nil {
					t.Fatalf("Resolve(%s) error = %v; the senior env must keep serving", senior, seniorErr)
				}
				if seniorToken != shared {
					t.Fatalf("Resolve(%s) = %q, want the configured value", senior, seniorToken)
				}
			})
		}
	}
	if want := len(envs) * (len(envs) - 1) / 2; pairs != want {
		t.Fatalf("exercised %d unordered pairs, want %d — every pair must be covered exactly once", pairs, want)
	}
}

// TestResolveAcceptsDistinctValues is the negative control for the test above:
// with every env set to its own value, every capability stays enabled. Guards
// against an overzealous check breaking clean deployments.
func TestResolveAcceptsDistinctValues(t *testing.T) {
	values := make(map[string]string, len(registry))
	for i, spec := range registry {
		values[spec.Env] = longEnough(string(rune('a' + i)))
	}
	getenv := envMap(values)

	for _, spec := range registry {
		token, err := Resolve(spec.Env, getenv)
		if err != nil {
			t.Fatalf("Resolve(%s) error = %v; distinct values must all resolve", spec.Env, err)
		}
		if token != values[spec.Env] {
			t.Fatalf("Resolve(%s) = %q, want the configured value", spec.Env, token)
		}
	}
}

// TestErrorsNeverContainTokenValue pins the logging contract: a reason names
// the ENV, never the secret. Reasons are logged at boot and, for some callers,
// surfaced in startup output — a value reaching one of them is a credential
// leak into the log pipeline.
func TestErrorsNeverContainTokenValue(t *testing.T) {
	const secret = "SUPER-SECRET-TOKEN-VALUE-DO-NOT-LOG-0000000000"
	const shortSecret = "SHORT-SECRET-DO-NOT-LOG"

	cases := []struct {
		name   string
		env    string
		getenv func(string) string
		value  string
	}{
		{
			name:   "collision",
			env:    DocsNotifyTokenEnv,
			getenv: envMap(map[string]string{NotifyInternalTokenEnv: secret, DocsNotifyTokenEnv: secret}),
			value:  secret,
		},
		{
			name:   "too short",
			env:    DriveInternalTokenEnv,
			getenv: envMap(map[string]string{DriveInternalTokenEnv: shortSecret}),
			value:  shortSecret,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.env, tc.getenv)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Fatalf("reason leaked the token value: %q", err.Error())
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("reason %q must name the env", err.Error())
			}
		})
	}
}

func TestResolveRejectsUnsetAndUnavailable(t *testing.T) {
	token, err := Resolve(DriveInternalTokenEnv, envMap(nil))
	if err == nil || token != "" {
		t.Fatalf("unset env = %q, %v; want a disabled capability", token, err)
	}
	var resolveErr *Error
	if !errors.As(err, &resolveErr) || resolveErr.Reason != ReasonUnset {
		t.Fatalf("unset reason = %+v, want %q", resolveErr, ReasonUnset)
	}

	token, err = Resolve(DriveInternalTokenEnv, nil)
	if err == nil || token != "" {
		t.Fatalf("nil getenv = %q, %v; want a disabled capability", token, err)
	}
	if !errors.As(err, &resolveErr) || resolveErr.Reason != ReasonUnavailable {
		t.Fatalf("nil getenv reason = %+v, want %q", resolveErr, ReasonUnavailable)
	}
}

// TestResolveRejectsUnregisteredEnv keeps a typo fail-closed: an env that is
// not in the registry cannot be compared against anything, so it must not be
// handed back as a usable credential.
func TestResolveRejectsUnregisteredEnv(t *testing.T) {
	const bogus = "OCTO_NOT_REGISTERED_TOKEN"
	token, err := Resolve(bogus, envMap(map[string]string{bogus: longEnough("z")}))
	if err == nil || token != "" {
		t.Fatalf("unregistered env = %q, %v; want a disabled capability", token, err)
	}
	var resolveErr *Error
	if !errors.As(err, &resolveErr) || resolveErr.Reason != ReasonUnregistered {
		t.Fatalf("unregistered reason = %+v, want %q", resolveErr, ReasonUnregistered)
	}
}

// TestLengthFloorRunsBeforeCollision pins the ordering documented in Resolve: a
// value below the floor is unusable on its own terms, so "lengthen this secret"
// is the whole fix and naming a sibling env only adds noise.
func TestLengthFloorRunsBeforeCollision(t *testing.T) {
	short := strings.Repeat("x", DefaultMinBytes-1)
	getenv := envMap(map[string]string{
		DriveInternalTokenEnv:  short,
		NotifyInternalTokenEnv: short,
	})
	_, err := Resolve(DriveInternalTokenEnv, getenv)
	var resolveErr *Error
	if !errors.As(err, &resolveErr) {
		t.Fatalf("error = %v, want *internaltoken.Error", err)
	}
	if resolveErr.Reason != ReasonTooShort {
		t.Fatalf("reason = %q, want %q (length gate runs first)", resolveErr.Reason, ReasonTooShort)
	}
	if strings.Contains(err.Error(), NotifyInternalTokenEnv) {
		t.Fatalf("a too-short value must not name a sibling env: %q", err.Error())
	}
}

// TestLegacyLengthWaiversAreExplicit documents which envs currently ship
// without the shared floor. Raising one is a deployment rollout decision (a
// short live value would be disabled on the next deploy), so the waiver list is
// pinned here rather than left to drift: adding a NEW env with MinBytes 0 fails
// this test, and lifting a waiver is a deliberate one-line edit in both places.
func TestLegacyLengthWaiversAreExplicit(t *testing.T) {
	waived := map[string]bool{
		NotifyInternalTokenEnv: true,
		DocsNotifyTokenEnv:     true,
		BotMentionTokenEnv:     true,
	}
	for _, spec := range Specs() {
		switch {
		case spec.MinBytes == 0 && !waived[spec.Env]:
			t.Fatalf("%s has no length floor and is not a documented legacy waiver; "+
				"new internal-token envs must use DefaultMinBytes", spec.Env)
		case spec.MinBytes != 0 && waived[spec.Env]:
			t.Fatalf("%s now enforces a floor of %d — drop it from this waiver list",
				spec.Env, spec.MinBytes)
		case spec.MinBytes != 0 && spec.MinBytes != DefaultMinBytes:
			t.Fatalf("%s floor = %d, want DefaultMinBytes (%d); one bar for operators to remember",
				spec.Env, spec.MinBytes, DefaultMinBytes)
		}
	}
}

// TestRegistryPrecedenceOrderIsStable pins the order itself. Position decides
// which live capability a misconfigured deployment loses, so reordering or
// inserting (rather than appending) is a behaviour change that must be
// deliberate. Appending a new env is expected and only needs this list extended.
func TestRegistryPrecedenceOrderIsStable(t *testing.T) {
	want := []string{
		NotifyInternalTokenEnv,
		DocsNotifyTokenEnv,
		BotMentionTokenEnv,
		DriveInternalTokenEnv,
	}
	got := Envs()
	if len(got) < len(want) {
		t.Fatalf("registry lost entries: got %v, want it to start with %v", got, want)
	}
	for i, env := range want {
		if got[i] != env {
			t.Fatalf("registry position %d = %s, want %s — reordering changes which "+
				"capability survives a collision; append new envs instead", i, got[i], env)
		}
	}
}

// TestSpecsAreWellFormed guards the registry itself: duplicate or unnamed
// entries would silently weaken Resolve's pairwise sweep.
func TestSpecsAreWellFormed(t *testing.T) {
	seen := make(map[string]struct{}, len(registry))
	for _, spec := range Specs() {
		if spec.Env == "" || spec.Capability == "" {
			t.Fatalf("spec %+v must name both an env and the capability it unlocks", spec)
		}
		if _, dup := seen[spec.Env]; dup {
			t.Fatalf("%s registered twice", spec.Env)
		}
		seen[spec.Env] = struct{}{}
		if !Registered(spec.Env) {
			t.Fatalf("Registered(%s) = false for a registered env", spec.Env)
		}
	}
	if Registered("OCTO_NOT_REGISTERED_TOKEN") {
		t.Fatal("Registered() accepted an unregistered env")
	}
}

func TestHeaderIsTheOneWireConvention(t *testing.T) {
	if Header != "X-Internal-Token" {
		t.Fatalf("Header = %q; every internal ingress in octo-server reads X-Internal-Token", Header)
	}
}

func TestValuesFeedsTheCrossCheck(t *testing.T) {
	first := longEnough("a")
	second := longEnough("b")
	getenv := envMap(map[string]string{
		NotifyInternalTokenEnv: first,
		BotMentionTokenEnv:     second,
		// DocsNotifyTokenEnv / DriveInternalTokenEnv unset — empties are skipped.
	})

	got := Values(getenv)
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("Values() = %v, want the two configured values in precedence order", got)
	}
}

// TestValuesPanicsOnNilGetenv pins the fail-closed choice behind Values and
// Collisions. Returning an empty slice would make
// registry.ValidateNotifyTokenExclusions(Values(nil)...) a no-op — the one gate
// here that aborts startup would silently pass a deployment where a fixed token
// equals a dynamic route credential.
func TestValuesPanicsOnNilGetenv(t *testing.T) {
	for name, call := range map[string]func(){
		"Values":     func() { Values(nil) },
		"Collisions": func() { Collisions(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s(nil) returned normally; a nil lookup must not silently "+
						"report an empty credential set", name)
				}
			}()
			call()
		})
	}
}

func TestCollisionsReportEveryPairByEnvName(t *testing.T) {
	shared := longEnough("s")
	getenv := envMap(map[string]string{
		NotifyInternalTokenEnv: shared,
		DocsNotifyTokenEnv:     shared,
		BotMentionTokenEnv:     shared,
		DriveInternalTokenEnv:  longEnough("d"),
	})

	got := Collisions(getenv)
	// Three envs sharing one value ⇒ 3 unordered pairs.
	if len(got) != 3 {
		t.Fatalf("Collisions() = %v, want 3 pairs", got)
	}
	for _, collision := range got {
		if collision.Senior == "" || collision.Junior == "" || collision.Senior == collision.Junior {
			t.Fatalf("malformed collision %+v", collision)
		}
		if collision.Senior == DriveInternalTokenEnv || collision.Junior == DriveInternalTokenEnv {
			t.Fatalf("%s holds a unique value but was reported in %+v", DriveInternalTokenEnv, collision)
		}
		// Senior/Junior must match the precedence order Resolve acts on,
		// otherwise the log line names the wrong survivor.
		_, seniorIndex, _ := lookup(collision.Senior)
		_, juniorIndex, _ := lookup(collision.Junior)
		if seniorIndex >= juniorIndex {
			t.Fatalf("collision %+v has the survivor and the disabled env the wrong way round", collision)
		}
	}

	if Collisions(envMap(map[string]string{NotifyInternalTokenEnv: shared})) != nil {
		t.Fatal("a single configured env cannot collide with anything")
	}
}

// TestCollisionsIgnoreUnsetEnvs is the case that would otherwise make every
// under-configured deployment look catastrophically broken: two envs that are
// both absent share the empty value but are not a collision.
func TestCollisionsIgnoreUnsetEnvs(t *testing.T) {
	if got := Collisions(envMap(nil)); got != nil {
		t.Fatalf("Collisions() = %v for an empty environment, want none", got)
	}
	for _, spec := range Specs() {
		if _, err := Resolve(spec.Env, envMap(nil)); err == nil {
			t.Fatalf("Resolve(%s) accepted an unset value", spec.Env)
		}
	}
}
