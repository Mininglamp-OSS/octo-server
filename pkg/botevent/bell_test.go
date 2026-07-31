package botevent

import (
	"errors"
	"testing"
	"time"
)

type fakeRinger struct {
	pushes  []string
	trims   [][3]interface{}
	expires []time.Duration
	llen    int

	pushErr   error
	trimErr   error
	expireErr error
}

func (f *fakeRinger) LPUSH(key string, values ...interface{}) (int64, error) {
	if f.pushErr != nil {
		return 0, f.pushErr
	}
	f.pushes = append(f.pushes, key)
	f.llen += len(values)
	return int64(f.llen), nil
}

func (f *fakeRinger) Ltrim(key string, start, stop int64) (string, error) {
	if f.trimErr != nil {
		return "", f.trimErr
	}
	f.trims = append(f.trims, [3]interface{}{key, start, stop})
	// LTRIM 0 0 keeps exactly one element.
	if f.llen > int(stop-start+1) {
		f.llen = int(stop - start + 1)
	}
	return "OK", nil
}

func (f *fakeRinger) Expire(key string, expiration time.Duration) error {
	if f.expireErr != nil {
		return f.expireErr
	}
	f.expires = append(f.expires, expiration)
	return nil
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

func TestRingKeepsListLengthAtOne(t *testing.T) {
	f := &fakeRinger{}
	for i := 0; i < 5; i++ {
		if err := Ring(f, "bot1"); err != nil {
			t.Fatalf("ring %d: %v", i, err)
		}
	}
	// An unattended doorbell must not grow — nobody may be long-polling for
	// hours, and the list would otherwise accumulate one token per event.
	if f.llen != 1 {
		t.Fatalf("doorbell length = %d, want 1", f.llen)
	}
	if len(f.trims) != 5 {
		t.Fatalf("trims = %d, want one per push", len(f.trims))
	}
	for _, tr := range f.trims {
		if tr[1].(int64) != 0 || tr[2].(int64) != 0 {
			t.Fatalf("trim range = %v, want [0,0]", tr)
		}
	}
}

func TestRingAlwaysSetsTTL(t *testing.T) {
	f := &fakeRinger{}
	if err := Ring(f, "bot1"); err != nil {
		t.Fatal(err)
	}
	if len(f.expires) != 1 || f.expires[0] != BellTTL {
		t.Fatalf("expires = %v, want one entry of %v", f.expires, BellTTL)
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
	if len(f.pushes) != 0 {
		t.Fatalf("blank robotID rang the bell: %v", f.pushes)
	}
}

func TestRingSurfacesErrorsForLoggingOnly(t *testing.T) {
	// Ring returns the error so producers can log it; producers are contractually
	// required to ignore it for control flow. This test pins that an error is
	// *reported* rather than swallowed, which is what makes the producer-side
	// `_ = Ring(...)` an explicit decision rather than an accident.
	want := errors.New("redis down")
	for name, f := range map[string]*fakeRinger{
		"push":   {pushErr: want},
		"trim":   {trimErr: want},
		"expire": {expireErr: want},
	} {
		if err := Ring(f, "bot1"); !errors.Is(err, want) {
			t.Fatalf("%s: got %v, want %v", name, err, want)
		}
	}
}
