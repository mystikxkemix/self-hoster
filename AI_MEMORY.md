# AI memory: self-hoster

## Decisions (2026-11-07, with Mike)
- One container only. A Go supervisor runs every project as a child process (no Docker socket, no per-project containers).
- MongoDB is NOT managed: Mike runs and configures it himself; projects get it through env vars.
- Routing is by port: each project gets its own port; Mike's existing nginx (outside the container) maps domains to ports. No reverse proxy inside.
- Code comes from git repos cloned and built inside the container (Node 24 + Go toolchains bundled).
- Two project types: `service` (spawned process) and `static` (website: build folder served by the Go binary itself, optional SPA fallback).
- Admin web app is React + Vite + TS in `web/`, built into `internal/api/ui/dist` and embedded with `//go:embed ui`; `ui/placeholder.html` is served when not built.
- Development happens in the dev container (`.devcontainer/`); toolchain versions must match the Dockerfile.

## Conventions / gotchas
- Every child runs in its own process group (Setpgid) so stop/kill reaches `npm -> node` grandchildren. tini is PID 1.
- `SELFHOSTER_*` env vars are stripped from project env (protects the admin password).
- `git clean -fd` (not `-fdx`) on redeploy: keeps ignored files (node_modules, build output) for faster rebuilds.
- Repo URLs may contain tokens: never log them.
- Build caches (GOCACHE, GOPATH, npm cache) live on /data to survive image upgrades.
- The API decodes with DisallowUnknownFields: the web client must send only spec fields (`toSpec` in web/src/lib/types.ts).
- Frontend fetches build absolute URLs from `location.origin`: relative fetch throws when the page is opened as http://user:pass@host.
- `docker-outside-of-docker` devcontainer feature fails to install on the trixie base image; not used.
- `pkill -f <pattern>` inside `bash -c '...'` kills its own shell when the pattern is in the command string; use a script file.
- zsh in this environment does not word-split `$VAR` in commands: use functions for curl helpers in tests.
