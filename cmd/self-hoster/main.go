// Command self-hoster runs personal projects (Node/TypeScript, Go, anything with a
// build and start command) as supervised processes inside a single container.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"selfhoster/internal/api"
	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dataDir := envOr("SELFHOSTER_DATA_DIR", "/data")
	addr := envOr("SELFHOSTER_ADDR", ":8080")
	user := envOr("SELFHOSTER_ADMIN_USER", "admin")
	password := os.Getenv("SELFHOSTER_ADMIN_PASSWORD")
	if password == "" {
		return errors.New("SELFHOSTER_ADMIN_PASSWORD must be set")
	}

	opts, err := storeOptions(addr, os.Getenv("SELFHOSTER_PORTS"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "projects"), 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "config.json"), opts)
	if err != nil {
		return err
	}
	sup := supervisor.New(dataDir)
	restore(st, sup)

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(st, sup, user, password),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("admin UI listening on %s", addr)
		serveErr <- srv.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serveErr:
		sup.Shutdown()
		return err
	case <-ctx.Done():
	}

	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	sup.Shutdown()
	return nil
}

// restore registers every project and brings back the ones that were running.
// Projects never cloned (e.g. a fresh data volume) are deployed from scratch.
func restore(st *store.Store, sup *supervisor.Supervisor) {
	for _, p := range st.List() {
		sup.Configure(p)
		if !p.Enabled {
			continue
		}
		var err error
		if sup.IsDeployed(p.Name) {
			err = sup.Start(p)
		} else {
			err = sup.Deploy(p)
		}
		if err != nil {
			log.Printf("restore %s: %v", p.Name, err)
		}
	}
}

// storeOptions reserves the admin port and, when set, restricts projects to the
// port range published by the container (e.g. "4000-4019").
func storeOptions(addr, portRange string) (store.Options, error) {
	var opts store.Options
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return opts, fmt.Errorf("SELFHOSTER_ADDR: %w", err)
	}
	if opts.ReservedPort, err = strconv.Atoi(port); err != nil {
		return opts, fmt.Errorf("SELFHOSTER_ADDR: %w", err)
	}
	if portRange == "" {
		return opts, nil
	}
	low, high, found := strings.Cut(portRange, "-")
	if !found {
		return opts, fmt.Errorf("SELFHOSTER_PORTS must look like 4000-4019, got %q", portRange)
	}
	if opts.MinPort, err = strconv.Atoi(strings.TrimSpace(low)); err != nil {
		return opts, fmt.Errorf("SELFHOSTER_PORTS: %w", err)
	}
	if opts.MaxPort, err = strconv.Atoi(strings.TrimSpace(high)); err != nil {
		return opts, fmt.Errorf("SELFHOSTER_PORTS: %w", err)
	}
	if opts.MinPort < 1 || opts.MaxPort > 65535 || opts.MinPort > opts.MaxPort {
		return opts, fmt.Errorf("SELFHOSTER_PORTS: invalid range %q", portRange)
	}
	return opts, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
