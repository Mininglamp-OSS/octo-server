package oidc

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStateStore_SaveAndConsume(t *testing.T) {
	s := newMemoryStateStore()
	ctx := context.Background()

	want := &StateData{
		Provider:     "aegis",
		CodeVerifier: "verifier",
		Nonce:        "nonce",
		IP:           "1.2.3.4",
		UserAgent:    "test-ua",
		ReturnTo:     "/home",
	}
	if err := s.Save(ctx, "state-1", want, time.Minute); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.Consume(ctx, "state-1")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got == nil {
		t.Fatal("expected data, got nil")
	}
	if got.CodeVerifier != want.CodeVerifier || got.Nonce != want.Nonce ||
		got.Provider != want.Provider || got.IP != want.IP ||
		got.UserAgent != want.UserAgent || got.ReturnTo != want.ReturnTo {
		t.Fatalf("data mismatch: got=%+v want=%+v", got, want)
	}
}

func TestMemoryStateStore_ConsumeIsOneShot(t *testing.T) {
	s := newMemoryStateStore()
	ctx := context.Background()

	_ = s.Save(ctx, "state-1", &StateData{Provider: "aegis"}, time.Minute)
	if _, err := s.Consume(ctx, "state-1"); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	got, err := s.Consume(ctx, "state-1")
	if !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("second consume err=%v want=ErrStateNotFound", err)
	}
	if got != nil {
		t.Fatal("second consume should return nil data")
	}
}

func TestMemoryStateStore_ConsumeExpired(t *testing.T) {
	s := newMemoryStateStore()
	ctx := context.Background()

	_ = s.Save(ctx, "state-1", &StateData{Provider: "aegis"}, 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if _, err := s.Consume(ctx, "state-1"); err != ErrStateNotFound {
		t.Fatalf("expired state should be not found, got %v", err)
	}
}

func TestMemoryStateStore_ConsumeUnknown(t *testing.T) {
	s := newMemoryStateStore()
	ctx := context.Background()
	if _, err := s.Consume(ctx, "no-such"); err != ErrStateNotFound {
		t.Fatalf("unknown state err=%v want=ErrStateNotFound", err)
	}
}

func TestNewState_Crypto(t *testing.T) {
	s1, err := NewRandomString(32)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewRandomString(32)
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Fatal("two random strings collided")
	}
	if len(s1) < 32 {
		t.Fatalf("string too short: %d", len(s1))
	}
}

func TestMemoryStateStore_ConsumeEmptyState(t *testing.T) {
	s := newMemoryStateStore()
	if _, err := s.Consume(context.Background(), ""); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("empty state should return ErrStateNotFound, got %v", err)
	}
}

func TestEncodeStateData_DoesNotMutateInput(t *testing.T) {
	d := &StateData{Provider: "aegis"}
	if !d.CreatedAt.IsZero() {
		t.Fatal("precondition: CreatedAt should start zero")
	}
	if _, err := encodeStateData(d); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !d.CreatedAt.IsZero() {
		t.Fatal("encodeStateData must not mutate caller's StateData.CreatedAt")
	}
}

func TestNewPKCEPair(t *testing.T) {
	verifier, challenge, err := NewPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier length out of RFC7636 bounds: %d", len(verifier))
	}
	if challenge == verifier {
		t.Fatal("challenge should not equal verifier")
	}

	v2, c2, _ := NewPKCEPair()
	if v2 == verifier || c2 == challenge {
		t.Fatal("PKCE pair should be unique per call")
	}
}
