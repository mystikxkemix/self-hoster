package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"selfhoster/internal/store"
)

// startStaticLocked serves the project's built folder on its port from inside the
// supervisor, so websites need no extra process. The caller holds opMu.
func (p *process) startStaticLocked(spec store.Project) error {
	root := filepath.Join(p.dir, spec.StaticDir)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		err = fmt.Errorf("static folder %q not found, check the build command output", spec.StaticDir)
		p.setError(err)
		return err
	}
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(spec.Port))
	if err != nil {
		p.setError(err)
		return err
	}
	site := &http.Server{Handler: staticHandler(root, spec.SPA), ReadHeaderTimeout: 10 * time.Second}
	started := time.Now()
	done := make(chan struct{})
	commit := p.currentCommit()
	p.mu.Lock()
	p.site = site
	p.done = done
	p.wantRunning = true
	p.status.State = StateRunning
	p.status.PID = 0
	p.status.StartedAt = &started
	p.status.Commit = commit
	p.status.LastError = ""
	p.mu.Unlock()
	p.logs.system("serving %s on port %d", spec.StaticDir, spec.Port)
	go func() {
		err := site.Serve(listener)
		p.mu.Lock()
		p.site = nil
		p.status.State = StateStopped
		p.status.StartedAt = nil
		if !errors.Is(err, http.ErrServerClosed) {
			p.status.LastError = err.Error()
		}
		p.mu.Unlock()
		p.logs.system("stopped serving")
		close(done)
	}()
	return nil
}

// stopStatic gracefully closes the static server and waits for it to exit.
func (p *process) stopStatic(site *http.Server, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := site.Shutdown(ctx); err != nil {
		_ = site.Close()
	}
	<-done
}

// staticHandler serves files from root. With spa enabled, unknown paths fall back to
// index.html so client-side routes survive a page reload.
func staticHandler(root string, spa bool) http.Handler {
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if spa {
			clean := path.Clean("/" + r.URL.Path)
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(clean))); errors.Is(err, fs.ErrNotExist) {
				w.Header().Set("Cache-Control", "no-cache")
				http.ServeFile(w, r, filepath.Join(root, "index.html"))
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
