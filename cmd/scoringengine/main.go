// Command scoringengine runs the competition scoring engine: it checks each
// configured service on its interval, deducts points for failures, and serves
// the public scoreboard and admin control API over HTTP.
//
// The engine always starts STOPPED with scoring PAUSED, even after a crash
// mid-competition. An admin must POST /login, then /engine/start (checks
// begin), then /scoring/resume (failures begin costing points).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/app"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second

	// shutdownTimeout bounds the whole graceful shutdown: draining in-flight
	// requests, then stopping every check routine (each bounded by its own
	// interval) and closing the database.
	shutdownTimeout = 30 * time.Second
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	dbPath := flag.String("db", "scoring.db", "path to the SQLite database file (its directory must exist)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	if err := run(*configPath, *dbPath, *addr); err != nil {
		log.Printf("scoringengine: %v", err)
		os.Exit(1)
	}
}

func run(configPath, dbPath, addr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(app.Options{ConfigPath: configPath, DBPath: dbPath})
	if err != nil {
		return err
	}

	// Listen before announcing readiness so a taken port fails fast.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Join(fmt.Errorf("listening on %s: %w", addr, err), a.Close())
	}
	srv := &http.Server{
		Handler:           a.Handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	log.Printf("session %q (id %d): %d service(s) configured from %s, database %s",
		a.Session.Name, a.Session.ID, len(a.Config.Services), configPath, dbPath)
	log.Printf("listening on %s", ln.Addr())
	log.Printf("engine is STOPPED and scoring is PAUSED; to begin, an admin must " +
		"POST /login, then POST /engine/start, then POST /scoring/resume")

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var errs []error
	select {
	case err := <-serveErr:
		errs = append(errs, fmt.Errorf("http server: %w", err))
		serveErr <- http.ErrServerClosed // let shutdown's drain below proceed
	case <-ctx.Done():
		log.Printf("shutdown requested; draining requests and stopping check routines (up to %v)", shutdownTimeout)
	}
	stop() // from here on a second signal kills the process immediately

	if err := shutdown(srv, serveErr, a); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	log.Printf("shutdown complete")
	return nil
}

// shutdown stops accepting requests and drains in-flight ones, then stops the
// engine (waiting for check routines to exit) and closes the database, all
// within shutdownTimeout. serveErr delivers srv.Serve's return value.
func shutdown(srv *http.Server, serveErr <-chan error, a *app.App) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var errs []error
	if err := srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("http shutdown: %w", err))
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		errs = append(errs, fmt.Errorf("http server: %w", err))
	}

	// Engine.Stop waits for every routine to exit; each is bounded by its
	// check's interval, but run it under the same deadline regardless so
	// shutdown can never hang. On timeout the process exits anyway: the
	// store forces the engine stopped on the next start.
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			errs = append(errs, err)
		}
	case <-ctx.Done():
		errs = append(errs, errors.New("timed out stopping the engine and closing the database"))
	}
	return errors.Join(errs...)
}
