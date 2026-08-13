package main

// `app cutover <domain> {preflight,activate,status}` — the shared operator
// surface for one-way data-plane cutovers, folded into the server binary.
//
// It replaces two standalone commands under tools/ (tools/msgextra-version for
// #627, tools/botevent-seq for #697) which the Dockerfile never built: the
// image ships only /home/app, so running either against production meant
// cross-compiling and copying a binary into a pod by hand, and a private
// deployment was guaranteed to arrive without them. `app session-rollout` had
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

	deadline, stopWatching := watchInterrupt(os.Stderr)
	defer stopWatching()

	rt := &cutoverRuntime{
		cfg:      cfg,
		ctx:      config.NewContext(cfg),
		deadline: deadline,
		floor:    *floor,
		yes:      *yes,
	}
	switch action {
	case "preflight":
		return domain.preflight(rt, out)
	case "activate":
		return domain.activate(rt, out)
	default:
		return domain.status(rt, out)
	}
}

// watchInterrupt installs the two-stage interrupt handling and returns the
// context the command's cancellable work runs under, plus a stop function the
// caller must defer.
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
func watchInterrupt(notice io.Writer) (context.Context, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		select {
		case <-signals:
			// Detach first: from here a second signal terminates by default,
			// which is the only way out of a phase that cannot be cancelled.
			signal.Stop(signals)
			cancel()
			fmt.Fprintln(notice, "\ninterrupt received: cancelling what can be cancelled; "+
				"press Ctrl-C again to terminate (an in-flight Redis scan cannot be interrupted)")
		case <-finished:
		}
	}()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(finished)
			signal.Stop(signals)
			cancel()
		})
	}
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
	vp, err := operatorConfigViper(path)
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
