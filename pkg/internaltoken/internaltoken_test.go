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
			if Yields(senior, junior) {
				// A Mutual env makes the pair symmetric, so there is no
				// survivor to assert; TestMutualCollisionsDisableBothSides
				// covers those.
				continue
			}
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
		t.Fatalf("visited %d unordered pairs, want %d — every pair must be covered exactly once", pairs, want)
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
		MarketplaceInternalTokenEnv,
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

// TestCollisionsNeverNameADarkEnvAsTheSurvivor is the multi-way case the
// obvious "report every unordered pair" implementation gets wrong. Three envs
// sharing one secret is the single-secret-for-everything misconfiguration this
// package exists to catch: Resolve disables the two juniors, so exactly two
// lines are correct, both naming the first env — the one still serving. A third
// line pairing the two juniors would advertise a dark env as the survivor, and
// an operator acting on it rotates the wrong secret.
func TestCollisionsNeverNameADarkEnvAsTheSurvivor(t *testing.T) {
	shared := longEnough("s")
	getenv := envMap(map[string]string{
		NotifyInternalTokenEnv: shared,
		DocsNotifyTokenEnv:     shared,
		BotMentionTokenEnv:     shared,
		DriveInternalTokenEnv:  longEnough("d"),
	})

	got := Collisions(getenv)
	if len(got) != 2 {
		t.Fatalf("Collisions() = %+v, want one line per DISABLED env (2), not one per unordered pair (3)", got)
	}
	for _, collision := range got {
		if collision.Senior != NotifyInternalTokenEnv {
			t.Fatalf("collision %+v names %s as the survivor; only the first env of the "+
				"equal-value group actually resolves", collision, collision.Senior)
		}
		if collision.Junior == NotifyInternalTokenEnv || collision.Junior == DriveInternalTokenEnv {
			t.Fatalf("collision %+v disabled the wrong env", collision)
		}
		// Every reported survivor must really resolve — the property the log
		// line at main.go claims.
		if !collision.SeniorServing {
			t.Fatalf("collision %+v reports its senior as dark, but %s resolves", collision, collision.Senior)
		}
		if _, err := Resolve(collision.Senior, getenv); err != nil {
			t.Fatalf("reported survivor %s does not resolve: %v", collision.Senior, err)
		}
		if _, err := Resolve(collision.Junior, getenv); err == nil {
			t.Fatalf("reported junior %s still resolves; it was not disabled", collision.Junior)
		}
	}

	if Collisions(envMap(map[string]string{NotifyInternalTokenEnv: shared})) != nil {
		t.Fatal("a single configured env cannot collide with anything")
	}
}

// TestCollisionsRespectTheLengthFloor pins that the boot report does not undo
// Resolve's length-before-collision ordering. A value below its floor is
// refused for length, so naming a sibling env would hand the operator a second
// cause to rule out for a token that could not authenticate either way.
func TestCollisionsRespectTheLengthFloor(t *testing.T) {
	short := strings.Repeat("x", DefaultMinBytes-1)
	getenv := envMap(map[string]string{
		NotifyInternalTokenEnv: short, // MinBytes 0 — legacy waiver, resolves fine
		DriveInternalTokenEnv:  short, // MinBytes 32 — refused for length, not collision
	})

	if _, err := Resolve(DriveInternalTokenEnv, getenv); err == nil {
		t.Fatal("precondition: the short drive token must be refused")
	}
	if got := Collisions(getenv); got != nil {
		t.Fatalf("Collisions() = %+v; a too-short value must not be reported as a collision, "+
			"or the boot report contradicts Resolve's own reason", got)
	}
}

func TestCollisionsPairSurvivorWithDisabled(t *testing.T) {
	shared := longEnough("s")
	getenv := envMap(map[string]string{
		NotifyInternalTokenEnv: shared,
		DocsNotifyTokenEnv:     shared,
	})
	got := Collisions(getenv)
	if len(got) != 1 {
		t.Fatalf("Collisions() = %+v, want exactly one", got)
	}
	if got[0].Senior != NotifyInternalTokenEnv || got[0].Junior != DocsNotifyTokenEnv {
		t.Fatalf("collision = %+v; the earlier-registered env is the survivor", got[0])
	}
	if !got[0].SeniorServing {
		t.Fatalf("collision = %+v; %s resolves and must be reported as serving", got[0], got[0].Senior)
	}
	if strings.Contains(got[0].Senior+got[0].Junior, shared) {
		t.Fatal("a collision must carry env names only, never a value")
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

// TestMutualCollisionsDisableBothSides pins the opt-out from precedence.
//
// A Mutual env's pairs are symmetric in both directions: it yields to every
// other entry, and every other entry yields to it regardless of registration
// order. That is the behaviour #827 shipped as a hand-written mirror branch in
// four modules; expressing it as one flag is only correct if it reproduces both
// halves, so both are asserted here.
func TestMutualCollisionsDisableBothSides(t *testing.T) {
	shared := longEnough("s")
	mutual := 0
	for _, spec := range Specs() {
		if !spec.Mutual {
			continue
		}
		mutual++
		for _, other := range Envs() {
			if other == spec.Env {
				continue
			}
			getenv := envMap(map[string]string{spec.Env: shared, other: shared})
			t.Run(spec.Env+"_and_"+other, func(t *testing.T) {
				if !Yields(spec.Env, other) || !Yields(other, spec.Env) {
					t.Fatalf("Yields is not symmetric for the Mutual pair (%s, %s)", spec.Env, other)
				}
				if token, err := Resolve(spec.Env, getenv); err == nil || token != "" {
					t.Fatalf("Resolve(%s) = %q, %v; the Mutual env must be disabled", spec.Env, token, err)
				}
				if token, err := Resolve(other, getenv); err == nil || token != "" {
					t.Fatalf("Resolve(%s) = %q, %v; a Mutual pair disables BOTH sides, "+
						"including an env registered earlier", other, token, err)
				}
			})
		}
	}
	if mutual == 0 {
		t.Skip("no Mutual entry in the registry; nothing to assert")
	}
}

// TestMutualIsOptInAndJustified keeps the flag from spreading by habit. Marking
// a pre-existing env Mutual takes a currently-serving ingress down on the next
// deploy of any installation carrying the collision, so the list is pinned.
func TestMutualIsOptInAndJustified(t *testing.T) {
	allowed := map[string]bool{MarketplaceInternalTokenEnv: true}
	for _, spec := range Specs() {
		if spec.Mutual && !allowed[spec.Env] {
			t.Fatalf("%s is marked Mutual but is not in the reviewed list; flipping an existing "+
				"env to Mutual disables a serving capability on the next deploy", spec.Env)
		}
		if !spec.Mutual && allowed[spec.Env] {
			t.Fatalf("%s lost its Mutual flag; #827 requires that pair to fail BOTH sides closed",
				spec.Env)
		}
	}
}
