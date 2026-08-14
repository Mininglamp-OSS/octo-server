package main

// `app cutover <domain> {preflight,activate,status}` — the shared operator
// surface for one-way data-plane cutovers, folded into the server binary.
//
// It replaces two standalone commands under tools/ (tools/msgextra-version for
// #627, tools/botevent-seq for #697) which the Dockerfile never built: the
// image carries the root-package binary plus assets/ and configs/ and nothing
// from tools/, so running either against production meant cross-compiling and
// copying a binary into a pod by hand, and a private deployment was guaranteed
// to arrive without them. `app session-rollout` had
// already made the same move for #733; this generalizes it.
//
// The three verbs mean the same thing in every domain:
//
//	preflight   read-only: current state, the evidence bounding the cutover
//	            floor, and the recommended floor. Safe anytime.
//	activate    the one-way flip; requires -yes, refuses floors outside the
//	            evidence-validated bounds, idempotent on re-run.
//	status      the cheap read: state row + expected-mode guard, no evidence
//	            scan. For "which mode is prod in" without paying for a
//	            keyspace walk.
//
// What each domain's evidence is, and what must be true around the flip
// (drains, write pauses, mirror publication), stays domain-specific — see
// docs/cutover-framework.md and the per-domain runbooks it links.
//
// Every invocation prints the endpoints it resolved before doing anything
// (the session-rollout lesson: on 2026-08-11 a misplaced config key sent a
// tool to 127.0.0.1:6379, where it scanned local test leftovers and reported
// complete — nothing in the output said so).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/cutover"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/go-sql-driver/mysql"
)

const cutoverCommand = "cutover"

// cutoverRuntime is what a domain action gets to work with: the resolved
// config/context plus the shared flag values.
type cutoverRuntime struct {
	cfg *config.Config
	ctx *config.Context
	// deadline carries operator interrupts (SIGINT/SIGTERM) into the DB calls,
	// so a wedged MySQL during an activation can be aborted rather than hanging
	// the procedure with writes drained.
	deadline context.Context
	floor    int64
	yes      bool
}

// cutoverDomain is one registered cutover. The three actions own their whole
// flow (including -yes gating and refusal wording) so that folding a tool into
// this dispatcher changes where it runs, not what it prints or refuses.
type cutoverDomain struct {
	name    string
	summary string
	// stateTable is the domain's singleton authority table. Registered here so
	// the schema conformance test can check every domain's DDL against the
	// framework template from one list, rather than each domain remembering to
	// write its own check.
	stateTable string
	// registerFlags adds domain-specific flags (e.g. botevent's -sample).
	registerFlags func(fs *flag.FlagSet)
	preflight     func(rt *cutoverRuntime, out io.Writer) error
	activate      func(rt *cutoverRuntime, out io.Writer) error
	status        func(rt *cutoverRuntime, out io.Writer) error
}

// cutoverDomainList returns the registered domains in display order. The
// session rollout (#733) is deliberately not here: it is a five-phase
// evidence-gated ladder with its own `app session-rollout` surface, not a
// two-state flip.
func cutoverDomainList() []*cutoverDomain {
	return []*cutoverDomain{
		msgextraCutoverDomain(),
		boteventCutoverDomain(),
	}
}

func cutoverUsage() string {
	var b strings.Builder
	b.WriteString("usage: app cutover <domain> <preflight|activate|status> [flags]\n\ndomains:\n")
	for _, d := range cutoverDomainList() {
		fmt.Fprintf(&b, "  %-10s %s\n", d.name, d.summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

// runCutoverCommand handles `app cutover <domain> <action> [flags]`. Like
// session-rollout it is dispatched before flag.Parse in main so subcommand
// flags are not eaten by the server's own -config flag, and domain/action are
// validated before the config is read so a typo costs a message rather than a
// dial timeout against whatever endpoint the config happens to name.
func runCutoverCommand(args []string, out io.Writer) error {
	if len(args) < 2 {
		return errors.New(cutoverUsage())
	}
	var domain *cutoverDomain
	for _, d := range cutoverDomainList() {
		if d.name == args[0] {
			domain = d
			break
		}
	}
	if domain == nil {
		return fmt.Errorf("unknown cutover domain %q\n%s", args[0], cutoverUsage())
	}
	action := args[1]
	switch action {
	case "preflight", "activate", "status":
	default:
		return fmt.Errorf("unknown cutover action %q (want preflight|activate|status)", action)
	}

	flags := flag.NewFlagSet(cutoverCommand+" "+domain.name+" "+action, flag.ContinueOnError)
	// The flag package would otherwise print usage and the parse error itself,
	// and main.go prints the returned error too — one typo, two messages.
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "configs/tsdd.yaml", "octo-server config file")
	floor := flags.Int64("floor", 0, "explicit cutover floor for activate (0 = use the preflight recommendation)")
	yes := flags.Bool("yes", false, "confirm a state-changing action (activate)")
	if domain.registerFlags != nil {
		domain.registerFlags(flags)
	}
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Asking for help is not a failure: print usage and exit 0.
			flags.SetOutput(out)
			flags.Usage()
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("cutover %s %s does not accept positional arguments", domain.name, action)
	}

	cfg, err := loadCutoverConfig(*configPath)
	if err != nil {
		return err
	}
	// Say where we are before doing anything (see the file header). Domains
	// that open their own Redis connection print its endpoint the same way.
	fmt.Fprintf(os.Stderr, "mysql: %s\n", redactedMySQLEndpoint(cfg.DB.MySQLAddr))

	watcher := watchInterrupt(os.Stderr)
	defer watcher.Stop()

	rt := &cutoverRuntime{
		cfg:      cfg,
		ctx:      config.NewContext(cfg),
		deadline: watcher.Context(),
		floor:    *floor,
		yes:      *yes,
	}
	switch action {
	case "preflight":
		err = domain.preflight(rt, out)
	case "activate":
		err = domain.activate(rt, out)
	default:
		err = domain.status(rt, out)
	}
	// Tag a cancellation with the signal behind it on the way out, so main can
	// exit 128+signum instead of guessing.
	return watcher.attributeInterrupt(err)
}

// interruptWatcher is a command's interrupt handling: the context its
// cancellable work runs under, and which signal (if any) stopped it.
//
// The signal is remembered rather than collapsed into "cancelled" because it is
// the only thing that distinguishes an operator's Ctrl-C from the platform
// terminating the pod — see cutoverExitCode.
type interruptWatcher struct {
	ctx      context.Context
	cancel   context.CancelFunc
	signals  chan os.Signal
	finished chan struct{}
	once     sync.Once

	mu       sync.Mutex
	received os.Signal
}

// Context is what every cancellable phase of the command runs under.
func (w *interruptWatcher) Context() context.Context { return w.ctx }

// Stop detaches the handler and releases the context. The caller must defer it;
// it is safe to call more than once, since an early return may also call it.
func (w *interruptWatcher) Stop() {
	w.once.Do(func() {
		close(w.finished)
		signal.Stop(w.signals)
		w.cancel()
	})
}

// deliver hands the watcher a signal as if the OS had. Test-only: production
// code lets signal.Notify fill the channel.
func (w *interruptWatcher) deliver(sig os.Signal) { w.signals <- sig }

// attributeInterrupt tags a cancellation with the signal that caused it, and
// leaves every other error exactly as it is.
//
// Only a cancellation is tagged: a refusal that happens to land in the same
// moment as a signal is still a refusal, and reporting it as an interrupt would
// tell a runbook wrapper the operator stopped a flip the command declined to
// perform.
func (w *interruptWatcher) attributeInterrupt(err error) error {
	if err == nil || !isCancellation(err) {
		return err
	}
	w.mu.Lock()
	sig := w.received
	w.mu.Unlock()
	if sig == nil {
		// Cancelled by something other than a signal — a deadline of the caller's
		// own, say. Nothing to attribute.
		return err
	}
	return &cutoverInterrupted{signal: sig, err: err}
}

// watchInterrupt installs the two-stage interrupt handling and returns the
// watcher; the caller must defer its Stop.
//
// Two-stage, and it has to be: installing a handler at all disables default
// termination for the whole command, but not every phase can observe a context.
// What does: the flip's own statements, and the MySQL evidence reads (both
// domains thread the deadline into them). What does not: the Redis sweeps —
// msgextra's messageExtraVersion:* SCAN/HSCAN and botevent's queue walk —
// because go-redis v6 takes no per-command context, so a sweep already in
// flight runs to completion whatever the operator presses.
//
// So the first signal cancels what is cancellable and immediately restores
// default handling, so a SECOND Ctrl-C terminates a scan that is not. Without
// that second stage, an operator interrupting a large keyspace scan — which for
// msgextra runs while the state row is held FOR UPDATE and every writer is
// blocked — would find the command ignoring every Ctrl-C and would have to go
// find the pod and SIGKILL it, with writes drained.
//
// The notice is gated on the signal channel rather than on the context being
// cancelled. An earlier revision used signal.NotifyContext and printed on
// <-ctx.Done(); since that stop function cancels before it detaches the
// handler, the deferred stop on the SUCCESS path woke the watcher and raced the
// process exit to announce an interrupt that never happened — landing directly
// under "ACTIVATED: allocator is now transactional", on the one surface whose
// entire design argument is that its output stays readable mid-incident.
func watchInterrupt(notice io.Writer) *interruptWatcher {
	w := newDetachedInterruptWatcher(notice)
	signal.Notify(w.signals, os.Interrupt, syscall.SIGTERM)
	return w
}

// newDetachedInterruptWatcher builds the watcher and starts it, but does not
// touch the process's signal disposition — the caller decides whether to wire
// it to the OS.
//
// The split exists for the tests. Asserting this watcher's behaviour by sending
// a real signal to the test binary is not a test but a hazard: the watcher
// detaches the instant it receives one, so a signal arriving in that window
// meets the default disposition and kills the process — and for SIGTERM that is
// silent, with no `--- FAIL` line to explain the dead package. Feeding the
// channel exercises every line that matters (the notice, the ordering, the
// recorded signal, the exit code) without betting the test binary on a race.
func newDetachedInterruptWatcher(notice io.Writer) *interruptWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &interruptWatcher{
		ctx:      ctx,
		cancel:   cancel,
		signals:  make(chan os.Signal, 1),
		finished: make(chan struct{}),
	}

	go func() {
		select {
		case sig := <-w.signals:
			// Detach first: from here a second signal terminates by default,
			// which is the only way out of a phase that cannot be cancelled.
			signal.Stop(w.signals)
			// Record before cancelling, for the same reason the notice is
			// written before cancelling: cancel releases the main goroutine,
			// which asks which signal this was on its way to the exit code.
			w.mu.Lock()
			w.received = sig
			w.mu.Unlock()
			// Notice BEFORE cancel, and the order is load-bearing. Cancelling
			// releases the main goroutine, which returns the cancellation up to
			// main and calls os.Exit — which does not wait for goroutines. A
			// notice printed after the cancel is in a race with process exit,
			// and the line at stake is the one telling the operator that a
			// second Ctrl-C will terminate.
			fmt.Fprintln(notice, "\ninterrupt received: cancelling what can be cancelled; "+
				"press Ctrl-C again to terminate (an in-flight Redis scan cannot be interrupted)")
			cancel()
		case <-w.finished:
		}
	}()
	return w
}

// cutoverInterrupted is a run that ended because the process was signalled. It
// carries the signal so the exit code can name it.
type cutoverInterrupted struct {
	signal os.Signal
	err    error
}

func (e *cutoverInterrupted) Error() string {
	return fmt.Sprintf("stopped by signal %v: %v", e.signal, e.err)
}

func (e *cutoverInterrupted) Unwrap() error { return e.err }

// cutoverExitCode maps a finished run to a process exit code.
//
// 128+signum is the shell convention, and the distinction it buys is real: a
// runbook wrapper reading 130 concludes an operator pressed Ctrl-C, while 143
// says the platform terminated the pod — `kubectl delete pod`, a job deadline,
// a deploy rolling the pod out from under an activation. After an interrupted
// one-way flip those call for different next steps, and the command previously
// reported 130 for both.
//
// Everything else is 1, including a refusal: "the activation was declined" must
// not read as "someone stopped it".
func cutoverExitCode(err error) int {
	if err == nil {
		return 0
	}
	var interrupted *cutoverInterrupted
	if errors.As(err, &interrupted) {
		if sig, ok := interrupted.signal.(syscall.Signal); ok && sig > 0 {
			return 128 + int(sig)
		}
		return 130
	}
	// A cancellation nobody could attribute to a signal. Still an interruption
	// from the caller's point of view, so keep the interrupt code rather than
	// reporting it as a refusal.
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}

// isCancellation reports whether err is the operator's interrupt (or a deadline
// running out) rather than something the database or Redis did wrong.
//
// The distinction decides what the operator is told: an evidence source that
// genuinely could not be read makes the totals a lower bound and must block an
// irreversible flip, while a cancellation means they asked to stop and should
// be answered as such — with the interrupt exit code, not a refusal citing
// unreadable evidence.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// redactedMySQLEndpoint renders a DSN as just the server and schema it points
// at.
//
// cfg.DB.MySQLAddr is a full go-sql-driver DSN — `user:password@tcp(host:port)
// /schema?params` — so echoing it verbatim would put the database password in
// the operator's scrollback, in `kubectl logs`, and in any audit capture of the
// cutover procedure. The endpoint is what the operator needs to see (that is
// the whole point of printing it); the credential is not.
//
// An unparseable DSN yields a fixed placeholder rather than the raw string:
// falling back to the input would put the password back exactly when the format
// is unexpected.
func redactedMySQLEndpoint(dsn string) string {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "(unparseable DSN; endpoint hidden to avoid leaking credentials)"
	}
	// ParseDSN normalizes before returning, so a successful parse always carries
	// an Addr (the tcp/unix default when the DSN omits one) — no empty-Addr case
	// to handle here.
	return cfg.Addr + "/" + cfg.DBName
}

// fprintRedisEndpoint echoes the Redis server a domain is about to read,
// resolved the same way the scanning code resolves it.
//
// Not cosmetic: on 2026-08-11 a misplaced config key (top-level `redisAddr`,
// which octo-lib ignores in favour of `db.redisAddr`) pointed a tool at
// 127.0.0.1:6379, where it scanned local test leftovers and reported a complete
// result. Cutover floors are computed from a Redis scan, so the same mistake
// here yields a floor that ignores every real client cursor.
func fprintRedisEndpoint(cfg *config.Config) {
	opts, err := octoredis.BuildOptions(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "redis: <unresolvable: %v>\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "redis: %s  db=%d\n", opts.Addr, opts.DB)
}

// loadCutoverConfig resolves the config file plus the same TS_* environment
// overrides the server applies.
func loadCutoverConfig(path string) (*config.Config, error) {
	vp, err := operatorConfigViper(path, os.Stderr)
	if err != nil {
		return nil, err
	}
	cfg := config.New()
	cfg.ConfigureWithViper(vp)
	return cfg, nil
}

// fprintCutoverGuard reports the expected-mode deployment guard next to the
// state in `status` output: the guard is half of every cutover's safety story
// (a lost state row fails closed only while the guard is armed), and its
// malformed-fails-closed behavior is invisible until it bites.
//
// It reports THIS PROCESS's environment, and says so. The guard is process-local
// configuration that no control plane can observe remotely, while both runbooks
// document running these commands from any machine with DB access — so a run
// from a laptop against an armed fleet would otherwise print "unset" and tell
// the operator the durable safety net is off when it is on.
//
// values comes from the domain package (its ExpectedModeSpellings), so this
// readout cannot disagree with the allocator about which values are valid.
func fprintCutoverGuard(out io.Writer, envName string, values map[string]int, modeName func(int) string) {
	raw := os.Getenv(envName)
	guard := cutover.ParseExpectedMode(raw, values)
	const scope = "in this process's env"
	switch {
	case !guard.IsSet():
		fmt.Fprintf(out, "guard %s: unset %s (no assertion here; arm it fleet-wide only after the flip is verified, and check a replica to confirm what the fleet has)\n", envName, scope)
	case guard.IsMalformed():
		fmt.Fprintf(out, "guard %s: MALFORMED value %q %s — a replica carrying this fails every allocation closed until it is fixed or unset\n", envName, raw, scope)
	default:
		expected, _ := guard.Expected()
		fmt.Fprintf(out, "guard %s: expects %s %s\n", envName, modeName(expected), scope)
	}
}

// cutoverModeName renders a mode using a domain's own spelling table, so the
// display name and the guard's accepted values cannot drift apart.
//
// The match is the lexicographically smallest spelling rather than the first
// one map iteration happens to yield: ParseExpectedMode accepts aliases (two
// spellings mapping to one mode is legal), and with Go's randomized map order
// an aliased domain would print a different name on successive runs of the same
// status command.
func cutoverModeName(spellings map[string]int, mode int) string {
	best := ""
	for name, value := range spellings {
		if value != mode {
			continue
		}
		if best == "" || name < best {
			best = name
		}
	}
	if best == "" {
		return fmt.Sprintf("unknown(%d)", mode)
	}
	return best
}
