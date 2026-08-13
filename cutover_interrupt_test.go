package main

// What an operator interrupt must actually do.
//
// The command's whole interrupt design rests on two claims: the first signal
// aborts what can be aborted and says so, and the exit code distinguishes "a
// human stopped this" from "the activation was refused". Both were false in
// ways only a test notices — a cancelled read counted as unreadable evidence,
// and a notice that raced process exit — so they are pinned here.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

// orderRecordingWriter captures whether the context was already cancelled at
// the moment the notice was written.
//
// Racing ctx.Done() against the write from the test goroutine would only ever
// be probabilistic — and would usually pass, because the watcher continues
// straight from cancel() to the write while the test still has to be
// scheduled. Recording the context's state from inside Write makes the ordering
// an observation rather than a race.
type orderRecordingWriter struct {
	mu            sync.Mutex
	buf           bytes.Buffer
	ctx           context.Context
	cancelledAtWr bool
	wrote         chan struct{}
}

func newOrderRecordingWriter() *orderRecordingWriter {
	return &orderRecordingWriter{wrote: make(chan struct{})}
}

func (w *orderRecordingWriter) setContext(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ctx = ctx
}

func (w *orderRecordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.ctx != nil {
		w.cancelledAtWr = w.ctx.Err() != nil
	}
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	close(w.wrote)
	return n, err
}

func (w *orderRecordingWriter) result() (text string, cancelledAtWrite bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String(), w.cancelledAtWr
}

// TestInterruptNoticeIsWrittenBeforeTheContextIsCancelled pins the ordering.
//
// Cancelling first releases the main goroutine, which returns the cancellation
// error up to main and calls os.Exit — and os.Exit does not wait for
// goroutines. So a notice printed after cancel() can simply be lost, on the one
// path where the operator needs it: it is the line that tells them the second
// stage is armed and a second Ctrl-C will terminate.
//
// Observing ctx.Done() is the moment the main goroutine would have been
// released, so the notice must already be there by then.
func TestInterruptNoticeIsWrittenBeforeTheContextIsCancelled(t *testing.T) {
	notice := newOrderRecordingWriter()
	watcher := watchInterrupt(notice)
	defer watcher.Stop()
	ctx := watcher.Context()
	notice.setContext(ctx)

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGINT))

	select {
	case <-notice.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the interrupt produced no notice at all")
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the interrupt never cancelled the context")
	}

	text, cancelledAtWrite := notice.result()
	require.Contains(t, text, "interrupt received")
	require.False(t, cancelledAtWrite,
		"the notice was written AFTER the context was cancelled. Cancelling releases the "+
			"main goroutine, which returns the error to main and calls os.Exit — and os.Exit "+
			"does not wait for goroutines, so this line (the one telling the operator a "+
			"second Ctrl-C terminates) can be lost on exactly the path that needs it")
}

// TestBoteventEvidenceAbortsOnInterruptInsteadOfCountingItAsUnreadable is the
// regression for a cancellation being laundered into evidence failure.
//
// gatherBoteventEvidence counts any sweep error into ev.failures and carries
// on. With the deadline threaded into those sweeps, an operator's Ctrl-C became
// two "source(s) could not be read" — after which preflight prints a full
// report and exits 0, and activate refuses with "Fix the read errors above and
// rerun". Both blame the database for what the operator just did, and neither
// reaches the exit-130 path.
//
// It also must not fall through into the Redis queue walk: that scan takes no
// per-command deadline, so entering it after an interrupt is precisely the
// uncancellable stretch the two-stage design exists to avoid.
func TestBoteventEvidenceAbortsOnInterruptInsteadOfCountingItAsUnreadable(t *testing.T) {
	cfg := cutoverInterruptTestConfig(t)
	ctx := config.NewContext(cfg)
	client := octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) { o.MaxRetries = 0 })
	defer client.Close()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	_, err := gatherBoteventEvidence(cancelled, ctx, client, &out, 0)
	require.ErrorIs(t, err, context.Canceled,
		"a cancelled deadline must abort the sweep, not be counted as an unreadable "+
			"evidence source — the operator would be told the database failed")
}

// TestBoteventStateReadsHonorTheDeadline covers the two botevent MySQL reads
// that are not evidence sweeps and were therefore missed when the deadline was
// threaded: the one behind `status`, and the mirror check that runs immediately
// before the flip with bot-event writes already paused.
//
// Both went through botevent.ReadState, which hardcodes context.Background, so
// against a wedged MySQL the first Ctrl-C did nothing at all — while
// watchInterrupt's docstring and docs/cutover-framework.md both claimed the
// MySQL reads observe the deadline in every domain.
func TestBoteventStateReadsHonorTheDeadline(t *testing.T) {
	cfg := cutoverInterruptTestConfig(t)
	ctx := config.NewContext(cfg)
	client := octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) { o.MaxRetries = 0 })
	defer client.Close()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("status", func(t *testing.T) {
		var out bytes.Buffer
		err := fprintBoteventState(cancelled, ctx, &out)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("pre-flip mirror check", func(t *testing.T) {
		err := refuseUnauthorizedBoteventMirror(cancelled, ctx, client)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestInterruptExitCodeCarriesTheSignalTheOperatorSent pins 128+signum.
//
// The command exited 130 for anything cancelled, which is the SIGINT
// convention. But these commands are as likely to be stopped by a SIGTERM —
// `kubectl delete pod`, a job deadline, a deploy rolling the pod out from under
// an activation — and a runbook wrapper that reads 130 concludes "a human
// pressed Ctrl-C" when what actually happened is that the platform reclaimed
// the pod mid-flip. Those call for different next steps, so the exit code has
// to tell them apart.
func TestInterruptExitCodeCarriesTheSignalTheOperatorSent(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		want int
	}{
		{name: "SIGINT", sig: syscall.SIGINT, want: 130},
		{name: "SIGTERM", sig: syscall.SIGTERM, want: 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watcher := watchInterrupt(io.Discard)
			defer watcher.Stop()

			require.NoError(t, syscall.Kill(syscall.Getpid(), tc.sig))
			select {
			case <-watcher.Context().Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%v never reached the watcher", tc.sig)
			}

			err := watcher.attributeInterrupt(watcher.Context().Err())
			require.Equal(t, tc.want, cutoverExitCode(err),
				"%v must exit %d (128+%d), so a runbook wrapper can tell an operator's "+
					"Ctrl-C apart from the platform terminating the pod mid-cutover",
				tc.sig, tc.want, int(tc.sig))
		})
	}
}

// A refusal is not an interrupt. The exit code is the only thing a wrapper
// script can branch on, and "the activation was refused because the floor was
// below the observed max" must not read as "someone stopped it".
func TestCutoverExitCodeLeavesOrdinaryFailuresAtOne(t *testing.T) {
	require.Equal(t, 1, cutoverExitCode(errors.New("refusing: -floor=5 is below the recommended floor 900")))
	require.Equal(t, 0, cutoverExitCode(nil))
}

func cutoverInterruptTestConfig(t *testing.T) *config.Config {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.DB.MySQLAddr = addr
	if redisAddr := os.Getenv("OCTO_TEST_REDIS_ADDR"); redisAddr != "" {
		cfg.DB.RedisAddr = redisAddr
	}
	probe := config.NewContext(cfg)
	if _, err := probe.DB().Exec("SELECT 1"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("no usable MySQL at %s: %v", addr, err)
		}
		t.Skipf("no usable MySQL at %s (%v) — set CI=true to make this a failure", addr, err)
	}
	return cfg
}
