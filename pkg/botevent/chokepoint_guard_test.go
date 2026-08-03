package botevent

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryBotEventQueueWriterRingsTheDoorbell pins the invariant that PR#685's
// review found broken: **every** site that ZADDs into the bot event queue must
// ring the doorbell afterwards, or /v1/bot/events long-poll will not wake
// promptly for the events that site produces.
//
// The original change wired only two of five writers because the docstring on
// enqueueBotEventGeneric claimed it was the shared chokepoint for all of them.
// It was not — saveRobotMessage and both notifyBotJoinedGroup variants carry
// their own inline GenSeq/ZAdd/Expire copies. A comment cannot hold this
// invariant; a source guard can.
//
// Modelled on pkg/redis's TestNoRawRedisClientOutsideChokepoint: scan production
// sources, find the writers structurally, and fail if one of them has no ring in
// the statements that follow.
func TestEveryBotEventQueueWriterRingsTheDoorbell(t *testing.T) {
	root := repoRootForGuard(t)

	// A queue writer is a ZAdd whose key is the bot event queue. Both spellings
	// in the tree are covered: the `robotEventPrefix` constant and the inline
	// "robotEvent:%s" literal.
	queueKey := regexp.MustCompile(`(robotEventPrefix|"robotEvent:%s")`)
	zaddCall := regexp.MustCompile(`\.ZAdd\(`)
	// Notify is the producer-facing entry point; Ring is the synchronous
	// primitive it wraps. Both count: a writer that calls Ring directly is
	// unusual (it puts a Redis round trip on the producer's goroutine — see
	// pkg/botevent/notify.go) but it does ring the bell, which is what this
	// guard is about.
	ringCall := regexp.MustCompile(`botevent\.(Notify|Ring)\(`)

	// How far after the ZADD the ring may appear. Generous enough for an error
	// branch plus a comment block, tight enough that a ring belonging to an
	// unrelated later site cannot be mistaken for this one's.
	const window = 24

	// Known writers at the time of writing: enqueueBotEventGeneric,
	// enqueueBotTypedEventGeneric, saveRobotMessage, and both
	// notifyBotJoinedGroup variants. Raise this when a writer is added.
	const minKnownQueueWriters = 5

	var violations []string
	matched := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".octospec":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if !zaddCall.MatchString(line) {
				continue
			}
			// Only ZADDs into the bot event queue matter; the key is normally
			// assigned to `key` a line or two above the call.
			ctxStart := i - 6
			if ctxStart < 0 {
				ctxStart = 0
			}
			if !queueKey.MatchString(strings.Join(lines[ctxStart:i+1], "\n")) {
				continue
			}
			matched++
			end := i + window
			if end > len(lines) {
				end = len(lines)
			}
			if ringCall.MatchString(strings.Join(lines[i:end], "\n")) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			violations = append(violations, rel+":"+itoaGuard(i+1))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A scanner that silently matches nothing would pass forever and protect
	// nothing — the same vacuity this guard exists to replace. If the key
	// spelling moves out of the lookback window, a new prefix constant appears,
	// or a writer switches to a pipeline or Lua, this fires instead of going
	// quietly green.
	if matched < minKnownQueueWriters {
		t.Fatalf("guard matched only %d bot-event queue writers, expected at least %d — "+
			"the scan has probably gone blind (key spelling, prefix constant, or a writer "+
			"moved to a pipeline/Lua). Fix the scan, do not lower this number without "+
			"confirming the writer really went away.", matched, minKnownQueueWriters)
	}

	if len(violations) > 0 {
		t.Fatalf("bot event queue ZADD without a botevent.Ring within %d lines:\n  %s\n\n"+
			"Every writer to robotEvent:{robotID} must ring the doorbell after a successful ZADD, "+
			"otherwise /v1/bot/events long-poll will not wake promptly for events from that site "+
			"(they surface only at the next chunk boundary). See pkg/botevent/bell.go.",
			window, strings.Join(violations, "\n  "))
	}
}

// TestGuardWouldCatchAnUnrungWriter proves the guard above can actually fail —
// a scanner that silently matches nothing would pass forever and protect nothing.
func TestGuardWouldCatchAnUnrungWriter(t *testing.T) {
	zaddCall := regexp.MustCompile(`\.ZAdd\(`)
	ringCall := regexp.MustCompile(`botevent\.(Notify|Ring)\(`)

	unrung := []string{
		`key := fmt.Sprintf("%s%s", rb.robotEventPrefix, robotID)`,
		`err = rb.ctx.GetRedisConn().ZAdd(key, float64(seq), payload)`,
		`if err != nil { return }`,
	}
	notified := append(append([]string{}, unrung...), `botevent.Notify(rb.ctx.GetConfig(), robotID)`)
	rung := append(append([]string{}, unrung...), `_ = botevent.Ring(rb.ctx.GetRedisConn(), robotID)`)

	if !zaddCall.MatchString(strings.Join(unrung, "\n")) {
		t.Fatal("the ZAdd pattern does not match a realistic writer; the guard would scan nothing")
	}
	if ringCall.MatchString(strings.Join(unrung, "\n")) {
		t.Fatal("the ring pattern matched a writer that does not ring")
	}
	// Both spellings must match, or renaming the producer entry point would
	// silently disarm the guard rather than fail it.
	if !ringCall.MatchString(strings.Join(notified, "\n")) {
		t.Fatal("the ring pattern does not match a writer that calls Notify")
	}
	if !ringCall.MatchString(strings.Join(rung, "\n")) {
		t.Fatal("the ring pattern does not match a writer that calls Ring")
	}
}

func repoRootForGuard(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (no go.mod found)")
		}
		dir = parent
	}
}

func itoaGuard(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
