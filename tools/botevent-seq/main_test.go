package main

import (
	"os"
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

// TestPreflightPagesQueueMembers guards the operator tool's production footprint.
// ZRANGE 0 -1 returns one unbounded Redis response and the previous implementation then
// retained a score-sized map. Rank pagination plus adjacent-score accounting bounds both.
func TestPreflightPagesQueueMembers(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)
	for _, forbidden := range []string{
		"ZRangeWithScores(k, 0, -1)",
		"make(map[float64]struct{}",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("preflight still uses unbounded queue accounting %q", forbidden)
		}
	}
	for _, required := range []string{"queueScanPageSize", "scanQueueScores"} {
		if !strings.Contains(text, required) {
			t.Errorf("preflight does not contain bounded paging helper %q", required)
		}
	}
}

func TestScanQueueScoresCountsDuplicatesAcrossPageBoundaries(t *testing.T) {
	members := make([]rd.Z, 0, queueScanPageSize+2)
	for i := int64(0); i < queueScanPageSize-1; i++ {
		members = append(members, rd.Z{Score: float64(i), Member: i})
	}
	// The duplicate straddles page 1 / page 2. An implementation that resets its
	// previous-score state per page silently misses exactly this case.
	members = append(members,
		rd.Z{Score: 10_000, Member: "a"},
		rd.Z{Score: 10_000, Member: "b"},
		rd.Z{Score: 10_001, Member: "c"},
	)
	var requests [][2]int64
	stats, err := scanQueueScores(func(start, stop int64) ([]rd.Z, error) {
		requests = append(requests, [2]int64{start, stop})
		if stop-start+1 > queueScanPageSize {
			t.Fatalf("requested unbounded page %d..%d", start, stop)
		}
		if start >= int64(len(members)) {
			return nil, nil
		}
		end := stop + 1
		if end > int64(len(members)) {
			end = int64(len(members))
		}
		return members[start:end], nil
	})
	if err != nil {
		t.Fatalf("scanQueueScores: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests=%v, want exactly two bounded pages", requests)
	}
	if stats.members != len(members) || stats.duplicates != 1 || stats.maxScore != 10_001 {
		t.Fatalf("stats=%+v, want members=%d duplicates=1 max=10001", stats, len(members))
	}
}

// TestInspectQueueScoresKeepsAtomicMaxWhenRankPagesShift reproduces the cutover
// evidence regression from review round 10. Pausing producers does not pause ACKs:
// removing members from the low end between rank pages shifts every later rank left,
// so the diagnostic walk can stop early after observing only scores 1..500 while the
// queue still contains scores through 2000. The activation floor must use a separate,
// single-command maximum and never the paged walk's incomplete view.
func TestInspectQueueScoresKeepsAtomicMaxWhenRankPagesShift(t *testing.T) {
	members := make([]rd.Z, 0, 2000)
	for i := 1; i <= 2000; i++ {
		members = append(members, rd.Z{Score: float64(i), Member: i})
	}

	pageCalls := 0
	stats, err := inspectQueueScores(
		func() ([]rd.Z, error) {
			return []rd.Z{members[len(members)-1]}, nil
		},
		func(start, stop int64) ([]rd.Z, error) {
			pageCalls++
			if start >= int64(len(members)) {
				return nil, nil
			}
			end := stop + 1
			if end > int64(len(members)) {
				end = int64(len(members))
			}
			page := append([]rd.Z(nil), members[start:end]...)
			if pageCalls == 1 {
				// Simulate ACKs removing scores 1..1600 after page 1. The next
				// rank request starts at 500 against a 400-member set and therefore
				// returns a short/empty page even though score 2000 still exists.
				members = members[1600:]
			}
			return page, nil
		},
	)
	if err != nil {
		t.Fatalf("inspectQueueScores: %v", err)
	}
	if stats.maxScore != 2000 {
		t.Fatalf("maxScore=%d, want atomic queue maximum 2000; rank paging understated the irreversible cutover evidence", stats.maxScore)
	}
	if stats.members != int(queueScanPageSize) {
		t.Fatalf("diagnostic walk read %d members, want %d to prove the rank view shifted", stats.members, queueScanPageSize)
	}
}
