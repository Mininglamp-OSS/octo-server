package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	rd "github.com/go-redis/redis"
)

// This is the first test file in tools/botevent-seq, and the reason it exists is that the
// tool carries the entire activation procedure, is the only component a human runs by hand
// at the cutover, and had produced two review findings across two rounds with no tests at
// all (#697 review round 7).
//
// judgeMirror is split out from the Redis/MySQL plumbing precisely so this matrix can be
// covered without either.

func TestJudgeMirror(t *testing.T) {
	cases := []struct {
		name        string
		activated   bool
		mirror      string
		absent      bool
		want        mirrorVerdict
		msgContains string
	}{
		{
			name:      "no mirror, not activated — the ordinary first flip",
			activated: false,
			absent:    true,
			want:      mirrorOK,
		},
		{
			name:      "no mirror, already activated — the documented mirror-repair case",
			activated: true,
			absent:    true,
			want:      mirrorOK,
		},
		{
			// The bug this file was added for: re-running `activate -yes` after a successful
			// flip is the normal state, and the previous revision died on it with a message
			// whose every clause was false, telling the operator to DEL the live mirror.
			name:        "correct mirror, already activated — must be a note, not a refusal",
			activated:   true,
			mirror:      botevent.MirrorValue(1),
			want:        mirrorNote,
			msgContains: "the authority agrees",
		},
		{
			// The case the check exists for: a mirror claiming activation the authority has
			// not granted.
			name:        "mirror claims activation, authority says no",
			activated:   false,
			mirror:      botevent.MirrorValue(1),
			want:        mirrorRefuse,
			msgContains: "NOT activated",
		},
		{
			name:        "bare incr against a legacy authority is still a claim",
			activated:   false,
			mirror:      "incr",
			want:        mirrorRefuse,
			msgContains: "NOT activated",
		},
		{
			name:        "malformed value is a note either way (not activated)",
			activated:   false,
			mirror:      "incr:not-a-number",
			want:        mirrorNote,
			msgContains: "not a valid mirror value",
		},
		{
			name:        "malformed value is a note either way (activated)",
			activated:   true,
			mirror:      "garbage",
			want:        mirrorNote,
			msgContains: "not a valid mirror value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := judgeMirror(tc.activated, tc.mirror, tc.absent)
			if got != tc.want {
				t.Fatalf("judgeMirror(activated=%v, mirror=%q, absent=%v) = %v, want %v (msg: %s)",
					tc.activated, tc.mirror, tc.absent, got, tc.want, msg)
			}
			if tc.msgContains != "" && !strings.Contains(msg, tc.msgContains) {
				t.Errorf("message does not mention %q; the operator reads this string during an "+
					"incident. Got: %s", tc.msgContains, msg)
			}
			if got == mirrorOK && msg != "" {
				t.Errorf("mirrorOK must carry no message, got %q", msg)
			}
		})
	}
}

// TestRefusalNeverTellsTheOperatorToDeleteALiveMirror pins the specific harm of the previous
// revision: it instructed a DEL of the mirror in a state where the mirror was correct.
//
// The instruction is right in the refusal case and must not appear in either note case.
func TestRefusalNeverTellsTheOperatorToDeleteALiveMirror(t *testing.T) {
	_, refuse := judgeMirror(false, botevent.MirrorValue(1), false)
	if !strings.Contains(refuse, "DEL the key") {
		t.Error("the refusal should tell the operator how to clear a genuinely unauthorized key")
	}
	for _, tc := range []struct {
		name      string
		activated bool
		mirror    string
	}{
		{"already activated with a correct mirror", true, botevent.MirrorValue(1)},
		{"malformed value", false, "garbage"},
	} {
		if _, msg := judgeMirror(tc.activated, tc.mirror, false); strings.Contains(msg, "DEL") {
			t.Errorf("%s: the message must not instruct a DEL — that is the case where the "+
				"mirror is live or about to be overwritten anyway. Got: %s", tc.name, msg)
		}
	}
}

// TestMaxSafeFloorKeepsScoresDistinct guards the other operator-facing bound.
//
// Sorted-set scores are float64, so above 2^53 distinct int64 ids stop having distinct
// scores — which would recreate the pagination skip and multi-member ack #697 removes, from
// the command meant to prevent them.
func TestMaxSafeFloorKeepsScoresDistinct(t *testing.T) {
	const float64ExactLimit = 1 << 53
	if maxSafeFloor >= float64ExactLimit {
		t.Fatalf("maxSafeFloor = %d is at or above 2^53 (%d), where int64 ids no longer have "+
			"distinct float64 scores", int64(maxSafeFloor), int64(float64ExactLimit))
	}
	// And it has to leave real room, or the cap becomes the thing that blocks an activation.
	if maxSafeFloor < 1<<40 {
		t.Fatalf("maxSafeFloor = %d leaves too little headroom above plausible ids",
			int64(maxSafeFloor))
	}
}

// TestMirrorWriteFailureStopsActivationAndExplainsPause is the round-8 D-4 regression.
//
// Committing the MySQL flip but failing to publish the mirror leaves replicas with cached
// negative beliefs on the legacy allocator until that cache expires. The operator command
// must therefore fail non-zero (the caller treats this returned error as fatal), and the
// message must tell the operator to keep writes paused instead of claiming the next
// allocation repairs the state immediately.
func TestMirrorWriteFailureStopsActivationAndExplainsPause(t *testing.T) {
	client := rd.NewClient(&rd.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  10 * time.Millisecond,
		ReadTimeout:  10 * time.Millisecond,
		WriteTimeout: 10 * time.Millisecond,
		MaxRetries:   0,
	})
	defer client.Close()

	err := writeMirror(client, 7)
	if err == nil {
		t.Fatal("mirror write failure returned nil; activate would exit 0 after an incomplete cutover")
	}
	for _, want := range []string{
		"authority is already activated",
		"keep bot-event writes paused",
		botevent.ModeKey,
		botevent.MirrorValue(7),
		botevent.NegativeBeliefTTL().String(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mirror write error does not contain %q: %v", want, err)
		}
	}
}

// TestActivationPreconditionsRemainVisibleWithYes pins the text printed for every activate
// invocation. -yes confirms the operator read the warning; it must not suppress the warning.
func TestActivationPreconditionsRemainVisibleWithYes(t *testing.T) {
	for _, want := range []string{
		"#704",
		"cutover floor",
		"EVERY replica",
		"writes are paused",
	} {
		if !strings.Contains(activationPreconditions, want) {
			t.Errorf("activation preconditions do not contain %q", want)
		}
	}
}
