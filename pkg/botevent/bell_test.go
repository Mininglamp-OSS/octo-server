package botevent

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
)

// The doorbell's whole contract lives inside a Lua script, so a test that only
// greps the script text can pass while the script does the wrong thing —
// `LTRIM key 0 1` and an EXPIRE aimed at the wrong key both survive a
// `strings.Contains` check. PR#685 review (yujiawei, P2-e) called that out after
// an earlier revision traded a behavioural test for a textual one.
//
// So the fake below is a *tiny Redis*, not a restatement of the script: it
// parses whatever `redis.call(...)` lines the script actually contains and
// applies ordinary Redis semantics to in-memory state. The script is data, the
// interpreter is generic, and the assertions are about the resulting state.
// An unrecognised command is a hard failure rather than a silent no-op, so a
// future command cannot slip past unmodelled.

type miniRedis struct {
	lists map[string][]string
	ttls  map[string]int64
}

func newMiniRedis() *miniRedis {
	return &miniRedis{lists: map[string][]string{}, ttls: map[string]int64{}}
}

var redisCallRe = regexp.MustCompile(`redis\.call\(([^)]*)\)`)

// eval runs the script's redis.call statements in order against the store.
func (m *miniRedis) eval(t *testing.T, script string, keys []string, argv []interface{}) {
	t.Helper()
	calls := redisCallRe.FindAllStringSubmatch(script, -1)
	if len(calls) == 0 {
		t.Fatalf("no redis.call found in script; the interpreter would assert nothing:\n%s", script)
	}
	for _, c := range calls {
		var parts []string
		for _, raw := range strings.Split(c[1], ",") {
			parts = append(parts, resolveArg(t, strings.TrimSpace(raw), keys, argv))
		}
		m.apply(t, parts[0], parts[1:])
	}
}

// resolveArg turns one Lua argument into its runtime value: KEYS[n] / ARGV[n]
// indirection, a quoted literal, or a bare number.
func resolveArg(t *testing.T, raw string, keys []string, argv []interface{}) string {
	t.Helper()
	switch {
	case strings.HasPrefix(raw, "KEYS["):
		return keys[luaIndex(t, raw)-1]
	case strings.HasPrefix(raw, "ARGV["):
		return fmt.Sprint(argv[luaIndex(t, raw)-1])
	case strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'"):
		return strings.Trim(raw, "'")
	default:
		return raw
	}
}

func luaIndex(t *testing.T, raw string) int {
	t.Helper()
	open := strings.Index(raw, "[")
	closeIdx := strings.Index(raw, "]")
	if open < 0 || closeIdx < open {
		t.Fatalf("malformed Lua subscript %q", raw)
	}
	n, err := strconv.Atoi(raw[open+1 : closeIdx])
	if err != nil {
		t.Fatalf("malformed Lua subscript %q: %v", raw, err)
	}
	return n
}

func (m *miniRedis) apply(t *testing.T, cmd string, args []string) {
	t.Helper()
	switch strings.ToUpper(cmd) {
	case "LPUSH":
		key := args[0]
		for _, v := range args[1:] {
			m.lists[key] = append([]string{v}, m.lists[key]...)
		}
	case "RPUSH":
		key := args[0]
		m.lists[key] = append(m.lists[key], args[1:]...)
	case "LTRIM":
		key := args[0]
		start, stop := listIndex(t, args[1]), listIndex(t, args[2])
		l := m.lists[key]
		if start < 0 {
			start += len(l)
		}
		if stop < 0 {
			stop += len(l)
		}
		if start < 0 {
			start = 0
		}
		if stop >= len(l) {
			stop = len(l) - 1
		}
		if start > stop || start >= len(l) {
			delete(m.lists, key)
			return
		}
		m.lists[key] = append([]string{}, l[start:stop+1]...)
	case "EXPIRE":
		key := args[0]
		if _, exists := m.lists[key]; !exists {
			return // Redis ignores EXPIRE on a missing key
		}
		m.ttls[key] = int64(listIndex(t, args[1]))
	case "DEL":
		for _, key := range args {
			delete(m.lists, key)
			delete(m.ttls, key)
		}
	default:
		t.Fatalf("the doorbell script issues %q, which this interpreter does not model — "+
			"model it rather than letting the command go unasserted", cmd)
	}
}

func listIndex(t *testing.T, raw string) int {
	t.Helper()
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("expected an integer argument, got %q: %v", raw, err)
	}
	return n
}

// fakeRinger records how Ring reached Redis and executes the script it sent.
// noscript makes the first EvalSha answer NOSCRIPT, which is how a cold Redis
// (or one after SCRIPT FLUSH) behaves.
type fakeRinger struct {
	store *miniRedis
	t     *testing.T

	evalSha  int
	eval     int
	loads    int
	noscript bool
	failWith error
}

func newFakeRinger(t *testing.T) *fakeRinger {
	return &fakeRinger{store: newMiniRedis(), t: t}
}

// NewCmdResult is go-redis's own test constructor — Cmd's error setter is
// unexported, so this is the supported way to fake a command outcome.
func (f *fakeRinger) run(script string, keys []string, args []interface{}) *rd.Cmd {
	if f.failWith != nil {
		return rd.NewCmdResult(nil, f.failWith)
	}
	f.store.eval(f.t, script, keys, args)
	return rd.NewCmdResult(int64(1), nil)
}

func (f *fakeRinger) Eval(script string, keys []string, args ...interface{}) *rd.Cmd {
	f.eval++
	return f.run(script, keys, args)
}

func (f *fakeRinger) EvalSha(sha1 string, keys []string, args ...interface{}) *rd.Cmd {
	f.evalSha++
	if f.noscript {
		f.noscript = false
		return rd.NewCmdResult(nil, errors.New("NOSCRIPT No matching script"))
	}
	if sha1 != ringScript.Hash() {
		f.t.Fatalf("EvalSha used %q, want the ring script's own hash %q", sha1, ringScript.Hash())
	}
	return f.run(ringSource, keys, args)
}

func (f *fakeRinger) ScriptExists(hashes ...string) *rd.BoolSliceCmd {
	return rd.NewBoolSliceResult([]bool{true}, nil)
}

func (f *fakeRinger) ScriptLoad(script string) *rd.StringCmd {
	f.loads++
	return rd.NewStringResult(ringScript.Hash(), nil)
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

func TestRingLeavesExactlyOneTokenHoweverOftenItRings(t *testing.T) {
	f := newFakeRinger(t)
	for i := 0; i < 25; i++ {
		if err := Ring(f, "bot1"); err != nil {
			t.Fatalf("ring %d: %v", i, err)
		}
	}
	// One token is all a waiter needs. An unattended bell must not grow with
	// event volume — this is the property `LTRIM 0 0` exists for, and it is
	// asserted from the resulting list, not from the script's text.
	got := f.store.lists[BellKey("bot1")]
	if len(got) != 1 {
		t.Fatalf("bell holds %d tokens after 25 rings, want exactly 1: %v", len(got), got)
	}
}

func TestRingExpiresTheBellKeyItPushedTo(t *testing.T) {
	f := newFakeRinger(t)
	if err := Ring(f, "bot1"); err != nil {
		t.Fatal(err)
	}
	// An EXPIRE aimed at the wrong key leaves the bell immortal while looking
	// entirely correct in the script source.
	ttl, ok := f.store.ttls[BellKey("bot1")]
	if !ok {
		t.Fatalf("no TTL on the bell key; TTLs recorded: %v", f.store.ttls)
	}
	if ttl != int64(BellTTL/time.Second) {
		t.Fatalf("bell TTL = %ds, want %ds", ttl, int64(BellTTL/time.Second))
	}
	if len(f.store.ttls) != 1 {
		t.Fatalf("the script expired keys other than the bell: %v", f.store.ttls)
	}
	// The TTL only has to outlive one hold; assert the invariant rather than the
	// literal, so lowering the hold ceiling cannot silently invert it.
	if BellTTL <= 30*time.Second {
		t.Fatalf("BellTTL %v must exceed the maximum long-poll hold", BellTTL)
	}
}

func TestRingIsOneRoundTripInSteadyState(t *testing.T) {
	f := newFakeRinger(t)
	for i := 0; i < 5; i++ {
		if err := Ring(f, "bot1"); err != nil {
			t.Fatalf("ring %d: %v", i, err)
		}
	}
	// The ring sits on the hottest bot delivery path, so it must cost one round
	// trip — not three commands, and not a re-sent script body per message.
	if f.evalSha != 5 {
		t.Fatalf("EvalSha calls = %d, want 5 (one per ring)", f.evalSha)
	}
	if f.eval != 0 {
		t.Fatalf("Eval calls = %d; a warm Redis must never re-send the script body", f.eval)
	}
}

func TestRingFallsBackToEvalOnColdRedis(t *testing.T) {
	f := newFakeRinger(t)
	f.noscript = true
	if err := Ring(f, "bot1"); err != nil {
		t.Fatalf("a cold Redis must not fail the ring: %v", err)
	}
	if f.eval != 1 {
		t.Fatalf("Eval calls = %d, want 1 after NOSCRIPT", f.eval)
	}
	// The fallback still has to leave the bell in the right state, not merely
	// return without an error.
	if got := f.store.lists[BellKey("bot1")]; len(got) != 1 {
		t.Fatalf("bell holds %d tokens after the EVAL fallback, want 1", len(got))
	}
	// And the next ring is back to one round trip.
	if err := Ring(f, "bot1"); err != nil {
		t.Fatal(err)
	}
	if f.eval != 1 {
		t.Fatalf("Eval calls = %d; the fallback must not become the steady state", f.eval)
	}
}

func TestRingIsNoOpOnEmptyInput(t *testing.T) {
	f := newFakeRinger(t)
	if err := Ring(nil, "bot1"); err != nil {
		t.Fatalf("nil ringer must be a no-op, got %v", err)
	}
	// A nil *rd.Client boxed in a non-nil Ringer is the shape a lazily built
	// client would arrive in; the interface-nil check alone cannot see it.
	var boxed *rd.Client
	if err := Ring(boxed, "bot1"); err != nil {
		t.Fatalf("nil *rd.Client must be a no-op, got %v", err)
	}
	if err := Ring(f, "   "); err != nil {
		t.Fatalf("blank robotID must be a no-op, got %v", err)
	}
	if f.evalSha != 0 || f.eval != 0 {
		t.Fatalf("blank robotID rang the bell: evalSha=%d eval=%d", f.evalSha, f.eval)
	}
}

func TestRingSurfacesErrorsForLogging(t *testing.T) {
	// Ring returns the error so producers can log it — and they do, at Warn.
	// This pins that a failure is *reported* rather than swallowed, which is
	// what makes a silently vanishing doorbell observable at all.
	want := errors.New("redis down")
	f := newFakeRinger(t)
	f.failWith = want
	if err := Ring(f, "bot1"); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}
