package internal_membership

import (
	"strings"
	"testing"
)

// envMap turns a fixture into the getenv function resolveMembershipInternalToken
// takes, so no test mutates the real process environment.
func envMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

const goodToken = "0123456789abcdef0123456789abcdef" // exactly 32 bytes

func TestTokenResolvesWhenWellFormed(t *testing.T) {
	got, err := resolveMembershipInternalToken(envMap(map[string]string{
		MembershipInternalTokenEnv: goodToken,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != goodToken {
		t.Fatalf("token mismatch")
	}
}

func TestTokenRefusedWhenUnset(t *testing.T) {
	got, err := resolveMembershipInternalToken(envMap(map[string]string{}))
	if err == nil {
		t.Fatal("want an error for an unset token")
	}
	if got != "" {
		t.Fatal("a refused token must resolve to the empty string so auth fails closed")
	}
}

func TestTokenRefusedWhenNoEnvAccessor(t *testing.T) {
	if _, err := resolveMembershipInternalToken(nil); err == nil {
		t.Fatal("want an error when the environment is unavailable")
	}
}

// TestTokenRefusedWhenTooShort also pins the boundary: exactly
// minInternalTokenBytes is accepted, one byte less is not.
func TestTokenRefusedWhenTooShort(t *testing.T) {
	short := strings.Repeat("a", minInternalTokenBytes-1)
	if _, err := resolveMembershipInternalToken(envMap(map[string]string{
		MembershipInternalTokenEnv: short,
	})); err == nil {
		t.Fatal("want an error for a token below the length floor")
	}
	exact := strings.Repeat("a", minInternalTokenBytes)
	if _, err := resolveMembershipInternalToken(envMap(map[string]string{
		MembershipInternalTokenEnv: exact,
	})); err != nil {
		t.Fatalf("exactly the floor must be accepted, got %v", err)
	}
}

// TestTokenRefusedOnSiblingCollision covers every sibling capability.
//
// It iterates siblingFixedTokenEnvs rather than a copy of it, on purpose: the
// previous version listed four envs while the central registry had grown to
// seven, so the suite certified a list that was already a subset — the test
// looked like coverage and was actually a snapshot. Iterating the real list means
// adding a sibling adds a case for free, and forgetting to add one is caught in
// main's TestModuleLocalRefusalsCoverTheCentralRegistry instead of here.
func TestTokenRefusedOnSiblingCollision(t *testing.T) {
	if len(siblingFixedTokenEnvs) < 6 {
		t.Fatalf("siblingFixedTokenEnvs shrank to %d entries; it must cover main.go's registry "+
			"minus this module's own env", len(siblingFixedTokenEnvs))
	}
	for _, sibling := range siblingFixedTokenEnvs {
		t.Run(sibling, func(t *testing.T) {
			_, err := resolveMembershipInternalToken(envMap(map[string]string{
				MembershipInternalTokenEnv: goodToken,
				sibling:                    goodToken,
			}))
			if err == nil {
				t.Fatalf("want an error when the token equals %s", sibling)
			}
			if !strings.Contains(err.Error(), sibling) {
				t.Errorf("error should name the colliding env, got %q", err.Error())
			}
		})
	}
}

// TestTokenErrorsNeverContainTheValue pins that a misconfiguration is
// diagnosable from logs without the log becoming a credential leak.
func TestTokenErrorsNeverContainTheValue(t *testing.T) {
	secret := strings.Repeat("s", minInternalTokenBytes)
	cases := []map[string]string{
		{MembershipInternalTokenEnv: "short"},
		{MembershipInternalTokenEnv: secret, notifyInternalTokenEnv: secret},
	}
	for _, env := range cases {
		_, err := resolveMembershipInternalToken(envMap(env))
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, v := range env {
			if v != "" && strings.Contains(err.Error(), v) {
				t.Fatalf("error leaked a token value: %q", err.Error())
			}
		}
	}
}

// TestLengthIsCheckedBeforeCollision pins the ordering: a too-short token must
// report its length, not that it collides, so a sibling's configuration state is
// never revealed for a value that could not authenticate anyway.
func TestLengthIsCheckedBeforeCollision(t *testing.T) {
	short := "abc"
	_, err := resolveMembershipInternalToken(envMap(map[string]string{
		MembershipInternalTokenEnv: short,
		notifyInternalTokenEnv:     short,
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), notifyInternalTokenEnv) {
		t.Fatalf("length must be reported before collision, got %q", err.Error())
	}
}

// TestEmptySiblingsDoNotCollide guards the case every deployment hits: most
// capabilities are unconfigured, so several siblings are the empty string and
// must not be treated as equal to a configured token.
func TestEmptySiblingsDoNotCollide(t *testing.T) {
	if _, err := resolveMembershipInternalToken(envMap(map[string]string{
		MembershipInternalTokenEnv: goodToken,
		notifyInternalTokenEnv:     "",
		driveInternalTokenEnv:      "",
	})); err != nil {
		t.Fatalf("unset siblings must not collide, got %v", err)
	}
}
