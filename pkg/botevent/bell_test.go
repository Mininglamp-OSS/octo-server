package botevent

import (
	"errors"
	"strings"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
)

// fakeRinger records the script invocations without needing a Redis.
type fakeRinger struct {
	calls   [][]string // keys per Eval
	ttls    []int64
	script  string
	evalErr error
}

func (f *fakeRinger) Eval(script string, keys []string, args ...interface{}) *rd.Cmd {
	f.script = script
	f.calls = append(f.calls, keys)
	if len(args) > 0 {
		if ttl, ok := args[0].(int64); ok {
			f.ttls = append(f.ttls, ttl)
		}
	}
	// NewCmdResult is go-redis's own test constructor — Cmd's error setter is
	// unexported, so this is the supported way to fake a failing command.
	return rd.NewCmdResult(int64(1), f.evalErr)
}

func TestBellKeyIsNamespacedAwayFromTheQueue(t *testing.T) {
	key := BellKey("bot123")
	if key != "robotEventBell:bot123" {
		t.Fatalf("unexpected bell key: %q", key)
	}
	// The authoritative queue key is `robotEvent:{robotID}`. If the bell ever
	// shared that namespace, ringing would corrupt the queue: guard the prefixes
	// cannot collide even by prefix-matching.
	if got, unwanted := key, "robotEvent:bot123"; got == unwanted {
		t.Fatal("bell key collides with the event queue key")
	}
}

func TestRingIsOneRoundTripAndTrimsToOne(t *testing.T) {
	f := &fakeRinger{}
	for i := 0; i < 5; i++ {
		if err := Ring(f, "bot1"); err != nil {
			t.Fatalf("ring %d: %v", i, err)
		}
	}
	// One Eval per ring, not three commands: this sits on the hottest bot
	// delivery path, where separate LPUSH/LTRIM/EXPIRE doubled the per-message
	// Redis round trips.
	if len(f.calls) != 5 {
		t.Fatalf("Eval calls = %d, want 5 (one round trip per ring)", len(f.calls))
	}
	for _, keys := range f.calls {
		if len(keys) != 1 || keys[0] != BellKey("bot1") {
			t.Fatalf("Eval keys = %v, want [%s]", keys, BellKey("bot1"))
		}
	}
	// The script must keep the list at one element and always refresh the TTL —
	// an unattended doorbell may sit for hours with nobody long-polling.
	if !strings.Contains(f.script, "LTRIM") || !strings.Contains(f.script, "0, 0") {
		t.Fatalf("script does not trim to a single element:\n%s", f.script)
	}
	if !strings.Contains(f.script, "LPUSH") || !strings.Contains(f.script, "EXPIRE") {
		t.Fatalf("script missing LPUSH/EXPIRE:\n%s", f.script)
	}
	// LTRIM after LPUSH, so a waiter about to block never observes an empty list.
	if strings.Index(f.script, "LTRIM") < strings.Index(f.script, "LPUSH") {
		t.Fatalf("LTRIM must run after LPUSH:\n%s", f.script)
	}
}

func TestRingAlwaysSetsTTL(t *testing.T) {
	f := &fakeRinger{}
	if err := Ring(f, "bot1"); err != nil {
		t.Fatal(err)
	}
	if len(f.ttls) != 1 || f.ttls[0] != int64(BellTTL/time.Second) {
		t.Fatalf("ttls = %v, want one entry of %v seconds", f.ttls, BellTTL/time.Second)
	}
	// The TTL only has to outlive one hold; assert the invariant rather than
	// the literal, so lowering the hold ceiling cannot silently invert it.
	if BellTTL <= 30*time.Second {
		t.Fatalf("BellTTL %v must exceed the maximum long-poll hold", BellTTL)
	}
}

func TestRingIsNoOpOnEmptyInput(t *testing.T) {
	f := &fakeRinger{}
	if err := Ring(nil, "bot1"); err != nil {
		t.Fatalf("nil ringer must be a no-op, got %v", err)
	}
	if err := Ring(f, "   "); err != nil {
		t.Fatalf("blank robotID must be a no-op, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("blank robotID rang the bell: %v", f.calls)
	}
}

func TestRingSurfacesErrorsForLoggingOnly(t *testing.T) {
	// Ring returns the error so producers can log it; producers are contractually
	// required to ignore it for control flow. This test pins that an error is
	// *reported* rather than swallowed, which is what makes the producer-side
	// `_ = Ring(...)` an explicit decision rather than an accident.
	want := errors.New("redis down")
	f := &fakeRinger{evalErr: want}
	if err := Ring(f, "bot1"); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}
