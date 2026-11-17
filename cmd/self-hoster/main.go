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
	"selfhoster/internal/autodeploy"
	"selfhoster/internal/mcp"
	"selfhoster/internal/oauth"
	"selfhoster/internal/settings"
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
	appsDir := envOr("SELFHOSTER_APPS_DIR", "/apps")
	addr := envOr("SELFHOSTER_ADDR", ":8080")
	// The MCP endpoint (for Claude) is off unless it gets its own listen address.
	mcpAddr := os.Getenv("SELFHOSTER_MCP_ADDR")
	user := envOr("SELFHOSTER_ADMIN_USER", "admin")
	password := os.Getenv("SELFHOSTER_ADMIN_PASSWORD")

	pollInterval, err := time.ParseDuration(envOr("SELFHOSTER_POLL_INTERVAL", "1m"))
	if err != nil {
		return fmt.Errorf("SELFHOSTER_POLL_INTERVAL: %w", err)
	}

	opts, err := storeOptions(addr, mcpAddr, os.Getenv("SELFHOSTER_PORTS"))
	if err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join(dataDir, "projects"), appsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	st, err := store.Open(filepath.Join(dataDir, "config.json"), opts)
	if err != nil {
		return err
	}
	// Settings saved from the admin UI live next to the projects, on the data volume.
	// The env vars above are their defaults.
	set, err := settings.Open(filepath.Join(dataDir, "settings.json"), settings.Defaults{
		AdminUser: user, AdminPassword: password, PollInterval: pollInterval,
	})
	if err != nil {
		return err
	}
	if password == "" && !set.HasStoredPassword() {
		return errors.New("SELFHOSTER_ADMIN_PASSWORD must be set (until a password is saved from the settings page)")
	}
	var provider *oauth.Provider
	if mcpAddr != "" {
		provider, err = oauth.Open(oauth.Config{
			Path:      filepath.Join(dataDir, "oauth.json"),
			PublicURL: os.Getenv("SELFHOSTER_MCP_URL"),
			Auth:      set,
		})
		if err != nil {
			return err
		}
	}
	sup := supervisor.New(dataDir, appsDir)
	restore(st, sup)

	servers := []*http.Server{{
		Addr: addr,
		Handler: api.New(st, sup, set, provider, api.Info{
			DataDir: dataDir, AppsDir: appsDir, Ports: os.Getenv("SELFHOSTER_PORTS"),
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}}
	if provider != nil {
		servers = append(servers, &http.Server{
			Addr:              mcpAddr,
			Handler:           mcp.Handler(provider, mcp.New(st, sup, os.Getenv("SELFHOSTER_PORTS"))),
			ReadHeaderTimeout: 10 * time.Second,
		})
	}
	serveErr := make(chan error, len(servers))
	for i, srv := range servers {
		go func() {
			if i == 0 {
				log.Printf("admin UI listening on %s", srv.Addr)
			} else {
				log.Printf("MCP endpoint listening on %s (path %s)", srv.Addr, oauth.ResourcePath)
			}
			serveErr <- srv.ListenAndServe()
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The poller stops before projects do, so no deploy starts during shutdown.
	pollCtx, stopPolling := context.WithCancel(ctx)
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		log.Printf("auto-deploy: checking remotes every %s (0 = disabled, editable in settings)", set.View().PollInterval)
		autodeploy.New(st, sup, set.PollInterval).Run(pollCtx)
	}()
	shutdown := func() {
		stopPolling()
		<-pollDone
		sup.Shutdown()
	}

	select {
	case err := <-serveErr:
		shutdown()
		return err
	case <-ctx.Done():
	}

	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	shutdown()
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

// storeOptions reserves the admin and MCP ports and, when set, restricts projects to
// the port range published by the container (e.g. "4000-4019").
func storeOptions(addr, mcpAddr, portRange string) (store.Options, error) {
	var opts store.Options
	reserved := map[string]string{"SELFHOSTER_ADDR": addr}
	if mcpAddr != "" {
		reserved["SELFHOSTER_MCP_ADDR"] = mcpAddr
	}
	for name, value := range reserved {
		_, port, err := net.SplitHostPort(value)
		if err != nil {
			return opts, fmt.Errorf("%s: %w", name, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return opts, fmt.Errorf("%s: %w", name, err)
		}
		opts.ReservedPorts = append(opts.ReservedPorts, n)
	}
	if len(opts.ReservedPorts) == 2 && opts.ReservedPorts[0] == opts.ReservedPorts[1] {
		return opts, errors.New("SELFHOSTER_MCP_ADDR must use a different port than SELFHOSTER_ADDR")
	}
	var err error
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
