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
	"syscall"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/cutover"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/go-sql-driver/mysql"
	"github.com/spf13/viper"
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

	// Operator interrupts abort the DB work instead of leaving the command
	// wedged against an unresponsive server mid-procedure.
	deadline, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	if cfg.Addr == "" {
		return cfg.DBName
	}
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
// overrides the server applies, returning an error instead of panicking so a
// bad path is an exit-1 message, not a stack trace.
func loadCutoverConfig(path string) (*config.Config, error) {
	vp := viper.New()
	vp.SetConfigFile(path)
	if err := vp.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()
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
func cutoverModeName(spellings map[string]int, mode int) string {
	for name, value := range spellings {
		if value == mode {
			return name
		}
	}
	return fmt.Sprintf("unknown(%d)", mode)
}
