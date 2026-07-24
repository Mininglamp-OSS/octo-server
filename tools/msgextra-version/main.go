// Command msgextra-version is the #627 operator tool for the transactional
// message_extra version allocator. It has two actions:
//
//	-action=preflight   (default) read-only: report the current allocator state
//	                    and the safe cutover floor (max already-issued version).
//	-action=activate    flip legacy→transactional under the drain barrier; needs
//	                    -yes. Uses -floor if given, else the preflight
//	                    recommendation. Refuses a floor below the observed max or
//	                    above MaxCutoverFloor.
//
// There is no "deactivate" action: rolling back to legacy cannot be done safely
// online (octo-lib's in-memory GenSeq HiLo cache would resume below the
// transactional high-water), so rollback is a documented maintenance-window
// coordinated procedure — see README.md §6.
//
// Activation is a runtime state-row flip, NOT a code deploy: it takes effect
// immediately across all replicas via the DB-authoritative state row. Run
// preflight first, verify the numbers, then activate.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/spf13/viper"
)

func main() {
	var configFile, action string
	var floor int64
	var yes bool
	flag.StringVar(&configFile, "config", "configs/tsdd.yaml", "octo-server config file")
	flag.StringVar(&action, "action", "preflight", "preflight | activate")
	flag.Int64Var(&floor, "floor", 0, "cutover floor for activate (0 = use the preflight recommendation)")
	flag.BoolVar(&yes, "yes", false, "confirm a state-changing action (activate)")
	flag.Parse()

	cfg, err := loadConfig(configFile)
	if err != nil {
		fatal(err)
	}
	ctx := config.NewContext(cfg)
	store := msgextraseq.New(ctx)

	switch action {
	case "preflight":
		runPreflight(store)
	case "activate":
		runActivate(store, floor, yes)
	default:
		fatal(fmt.Errorf("unsupported -action %q (want preflight|activate)", action))
	}
}

func runPreflight(store *msgextraseq.Store) {
	res, err := store.Preflight()
	if err != nil {
		fatal(err)
	}
	printPreflight(res)
}

func runActivate(store *msgextraseq.Store, floor int64, yes bool) {
	// Always show the preflight first so the operator sees what they are acting on.
	res, err := store.Preflight()
	if err != nil {
		fatal(err)
	}
	printPreflight(res)
	if res.CurrentMode == msgextraseq.ModeTransactional {
		fmt.Println("\nalready transactional — nothing to do")
		return
	}
	if floor == 0 {
		floor = res.RecommendedFloor
		fmt.Printf("\n-floor not set: using preflight recommendation %d\n", floor)
	}
	if floor < res.RecommendedFloor {
		fatal(fmt.Errorf("refusing: -floor=%d is below the recommended floor %d (would risk reissuing an existing version)", floor, res.RecommendedFloor))
	}
	if floor > msgextraseq.MaxCutoverFloor {
		fatal(fmt.Errorf("refusing: -floor=%d exceeds the maximum %d (would make every reservation fail ErrOverflow)", floor, msgextraseq.MaxCutoverFloor))
	}
	if !yes {
		fatal(fmt.Errorf("activate is a state change across all replicas — re-run with -yes to confirm (floor=%d)", floor))
	}
	flipped, err := store.Activate(floor)
	if err != nil {
		fatal(err)
	}
	if !flipped {
		fmt.Println("\nno change — allocator was already transactional")
		return
	}
	fmt.Printf("\nACTIVATED: allocator is now transactional (cutover_floor=%d). Verify versions are monotonic and rerun preflight to confirm mode=transactional.\n", floor)
}

func printPreflight(res msgextraseq.PreflightResult) {
	fmt.Printf("current: mode=%s epoch=%d cutover_floor=%d\n", modeName(res.CurrentMode), res.CurrentEpoch, res.CurrentFloor)
	fmt.Printf("evidence: max(message_extra.version)=%d max(legacy seq boundary)=%d\n", res.MaxMessageExtraVersion, res.MaxLegacySeqBoundary)
	fmt.Printf("recommended cutover_floor=%d\n", res.RecommendedFloor)
}

func modeName(mode int) string {
	switch mode {
	case msgextraseq.ModeLegacy:
		return "legacy"
	case msgextraseq.ModeTransactional:
		return "transactional"
	default:
		return fmt.Sprintf("unknown(%d)", mode)
	}
}

func loadConfig(path string) (*config.Config, error) {
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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "msgextra-version:", err)
	os.Exit(1)
}
