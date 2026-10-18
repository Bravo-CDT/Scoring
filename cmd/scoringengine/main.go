// Command scoringengine runs the competition scoring engine: it checks every
// configured service once per shared scoring round, deducts points for
// failures, and serves the public scoreboard and admin control API over HTTP.
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
	"path/filepath"
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
	// requests, then stopping the round loop (bounded by a check's short
	// grace period) and the engine, and closing the database.
	shutdownTimeout = 30 * time.Second
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	dbPath := flag.String("db", "scoring.db", "path to the SQLite database file (its directory must exist)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	webDir := flag.String("web", "./web/dist", "directory of the built frontend to serve (see make build); empty serves only the API")
	flag.Parse()

	if err := run(*configPath, *dbPath, *addr, *webDir); err != nil {
		log.Printf("scoringengine: %v", err)
		os.Exit(1)
	}
}

func run(configPath, dbPath, addr, webDir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(app.Options{ConfigPath: configPath, DBPath: dbPath, WebDir: webDir})
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
	logFrontend(webDir)
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

// logFrontend says where the frontend is served from, or warns that it will
// not be. A missing build is not fatal: the API works without it, and the
// scoreboard pages answer 404 until it is built.
func logFrontend(webDir string) {
	if webDir == "" {
		log.Printf("no -web directory; serving the API only")
		return
	}
	index := filepath.Join(webDir, "index.html")
	if _, err := os.Stat(index); err != nil {
		log.Printf("WARNING: frontend not found (%v); serving the API only until it is built (make build)", err)
		return
	}
	log.Printf("serving the frontend from %s", webDir)
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

	// Close waits for the round loop to exit, which is bounded by a check's
	// grace period, but run it under the same deadline regardless so
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
