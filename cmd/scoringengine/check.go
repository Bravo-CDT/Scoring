package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
)

const checkUsage = `usage: scoringengine check [-config config.yaml] [service ...]

Runs the configured checks once, concurrently, and prints one line per service:
name, PASS or FAIL, latency, and detail. With no service names it runs every
configured service. Each check gets the same timeout a scoring round gives it
(round_interval_seconds), so the answer is the one the engine would record.

It touches no database and no running engine, so it is safe to run alongside a
live one. ADMIN_PASSWORD_HASH need not be set. Run it from the repository root
(like make run): script checks are resolved relative to ./checks.

Exit status: 0 if every check passed, 1 if any failed, 2 on a usage or config
error (an unknown service name included).

Flags:
`

// runCheckCmd implements `scoringengine check` and returns its exit status.
func runCheckCmd(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), checkUsage)
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "config.yaml", "path to the config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, unset, err := config.LoadForChecks(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scoringengine check: %v\n", err)
		return 2
	}
	for _, name := range unset {
		fmt.Fprintf(os.Stderr, "warning: environment variable %s is not set; ${%s} is used as literal text\n", name, name)
	}

	services, ok := selectServices(cfg.Services, fs.Args())
	if !ok {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	timeout := time.Duration(cfg.RoundIntervalSeconds) * time.Second
	registry := checks.NewDefaultRegistry()
	results := make([]checks.Result, len(services))
	latencies := make([]time.Duration, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		// Config validation guarantees the check type is registered.
		fn, _ := registry.Get(svc.CheckType)
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			results[i] = scheduler.RunCheck(ctx, timeout, fn, svc.Host, svc.Params)
			latencies[i] = time.Since(start)
		}()
	}
	wg.Wait()

	status := 0
	for i, svc := range services {
		verdict := "PASS"
		if !results[i].Success {
			verdict = "FAIL"
			status = 1
		}
		fmt.Printf("%-12s %s  %6s  %s\n", svc.Name, verdict, latencies[i].Round(time.Millisecond), results[i].Detail)
	}
	return status
}

// selectServices returns the services named in names, in config order, or
// every service if names is empty. It reports each unknown name on stderr and
// returns false if there were any.
func selectServices(all []config.ServiceConfig, names []string) ([]config.ServiceConfig, bool) {
	if len(names) == 0 {
		return all, true
	}
	configured := make([]string, len(all))
	for i, svc := range all {
		configured[i] = svc.Name
	}
	ok := true
	for _, name := range names {
		if !slices.Contains(configured, name) {
			fmt.Fprintf(os.Stderr, "unknown service %q (configured: %s)\n", name, strings.Join(configured, ", "))
			ok = false
		}
	}
	if !ok {
		return nil, false
	}
	var selected []config.ServiceConfig
	for _, svc := range all {
		if slices.Contains(names, svc.Name) {
			selected = append(selected, svc)
		}
	}
	return selected, true
}
