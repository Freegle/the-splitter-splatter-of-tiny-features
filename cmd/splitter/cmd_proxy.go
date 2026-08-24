// Adapted from seifghazi/claude-code-proxy (MIT), commit 02c9c766.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/proxy"
	"github.com/freegle/splitter/internal/store"
)

// shutdownDrainTimeout bounds how long graceful shutdown waits for
// in-flight requests to finish, and separately how long it then waits for
// the async logger to drain its buffered channel, on SIGINT/SIGTERM.
const shutdownDrainTimeout = 5 * time.Second

// idleTimeout is the only timeout set on the client-facing http.Server: no
// read or write timeout, so a long SSE stream is never cut off.
const idleTimeout = 120 * time.Second

func init() {
	register("proxy", runProxy)
}

// runProxy starts the Phase 1 pass-through logging proxy and blocks until
// SIGINT or SIGTERM, then shuts down gracefully: stop accepting new
// connections and let in-flight requests finish, then drain the logger's
// buffered channel, each phase bounded by shutdownDrainTimeout.
func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.toml (overrides $SPLITTER_CONFIG and the default location)")
	listenOverride := fs.String("listen", "", "override the configured listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	listen := resolveProxyListen(cfg, *listenOverride)

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening store at %s: %w", cfg.DBPath, err)
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		return fmt.Errorf("migrating store: %w", err)
	}

	proxySrv, err := proxy.New(proxy.Config{
		Upstream: cfg.Upstream,
		DB:       db,
		RepoPath: cfg.RepoPath,
	})
	if err != nil {
		return fmt.Errorf("building proxy: %w", err)
	}

	httpSrv := &http.Server{
		Addr:        listen,
		Handler:     proxySrv,
		IdleTimeout: idleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("splitter: proxy: listening on %s, forwarding to %s", listen, cfg.Upstream)
		err := httpSrv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			serveErr <- fmt.Errorf("listen: %w", err)
			return
		}
		serveErr <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		return err
	case sig := <-sigCh:
		log.Printf("splitter: proxy: received %s, shutting down", sig)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("splitter: proxy: http shutdown: %v", err)
	}
	shutdownCancel()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer drainCancel()
	if err := proxySrv.Close(drainCtx); err != nil {
		log.Printf("splitter: proxy: logger drain: %v", err)
	}

	if dropped := proxySrv.Dropped(); dropped > 0 {
		log.Printf("splitter: proxy: %d capture records were dropped during this run (logger channel full)", dropped)
	}

	return <-serveErr
}

// resolveProxyListen returns override when set, else cfg.Listen.
func resolveProxyListen(cfg *config.Config, override string) string {
	if override != "" {
		return override
	}
	return cfg.Listen
}
