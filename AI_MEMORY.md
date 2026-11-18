# AI memory: self-hoster

## Decisions (2026-11-07, with Mike)
- One container only. A Go supervisor runs every project as a child process (no Docker socket, no per-project containers).
- MongoDB is NOT managed: Mike runs and configures it himself; projects get it through env vars.
- Routing is by port: each project gets its own port; Mike's existing nginx (outside the container) maps domains to ports. No reverse proxy inside.
- Code comes from git repos cloned and built inside the container (Node 24 + Go toolchains bundled).
- Two project types: `service` (spawned process) and `static` (website: build folder served by the Go binary itself, optional SPA fallback).
- Admin web app is React + Vite + TS in `web/`, built into `internal/api/ui/dist` and embedded with `//go:embed ui`; `ui/placeholder.html` is served when not built.
- Deploy on push = polling, decided 2026-11-07 (no webhook, no self-hosted runner): `internal/autodeploy` runs `git ls-remote` per enabled+autoDeploy project every SELFHOSTER_POLL_INTERVAL (1m). Mike's repos are private, under his personal account (madmike75), so a GitHub runner would have to be registered per repo.
- Failed builds are not retried per commit (`lastTried`, in memory); check errors are logged once per distinct error.
- Each project gets a persistent folder SELFHOSTER_APPS_DIR/<name> (default /apps, Mike's choice), passed as DATA_DIR to build and run; user env may override it. Never deleted, not even by project delete. Checkouts under /data/projects are disposable.
- budget-app (github.com/mystikxkemix/budget-app) is a single Next.js 15 + SQLite app, NOT Nx. Its `next build` races on SQLite (module-level WAL pragma in lib/db/client.ts, many build workers): run `node scripts/migrate.mjs` before `npm run build`.
- Admin settings (username, hashed password, poll interval, link host) live in SELFHOSTER_DATA_DIR/settings.json (internal/settings); env vars are defaults. Basic auth checks go through settings.Manager.Authenticate, which caches the last verified password digest so PBKDF2 (600k iterations) runs once per password, not per request.
- The poller re-reads its interval every second (dynamic from settings). Projects have an optional public `url` used by the UI's Open button; otherwise http://<linkHost or page host>:<port>.
- UI layout: the page never scrolls on desktop; .project-list and .main scroll independently (min-height: 0 on flex/grid children). Below 760px it falls back to a normal scrolling page.
- Browser UI checks: playwright-core (in the scratchpad, not the repo) driving the host Chrome with channel "chrome" and httpCredentials.
- ZimaOS: root filesystem is read-only (no /root/.docker), the user (kemix, HOME=/DATA) is not in the docker group. Private GHCR pulls work with `sudo docker --config /DATA/.docker pull ghcr.io/mystikxkemix/self-hoster:latest` after a user-level `docker login ghcr.io` (classic token, read:packages). Verified 2026-11-08.
- ZimaOS install (running since 2026-11-08): imported compose with `network_mode: host` (the ZimaOS importer rejects port ranges), admin on :8090 (SELFHOSTER_ADDR), projects 4000-4019, volumes /DATA/AppData/self-hoster/{data,apps}.
- MCP for Claude (2026-11-17, Mike's choices): tools = operate + edit, NO delete; served by the admin server on the admin port (Mike's choice 2026-11-18, replaced a separate port). On when SELFHOSTER_MCP_URL is set. `mcp.Register` mounts the OAuth + /mcp routes on a root mux before the basic-auth admin mux, so they skip basic auth. nginx must forward only those paths (README has the regex), or the admin UI becomes public. `/mcp` is registered per method (GET/POST/DELETE): a method-less "/mcp" panics against "GET /". Stdlib-only, no MCP SDK: `internal/oauth` (OAuth 2.1 AS: RFC 9728/8414 metadata, open DCR capped at 100 clients, PKCE S256 required, rotating refresh tokens, hashed tokens in data/oauth.json, consent = admin user/password with a global lockout) and `internal/mcp` (stateless Streamable HTTP, JSON responses only, no SSE).
- MCP tools never return env values (only `envKeys`) and redact repo URL credentials; update_project/set_env are patches, and a repo containing `***@` is rejected so a redacted URL cannot overwrite the real token.
- OAuth tokens are bound to the resource URL (SELFHOSTER_MCP_URL + /mcp): changing that URL disconnects every client.
- Project operations (create, save + restart if running, delete, deploy/start/stop/restart and the Enabled flag) live in `internal/projects`, shared by the admin API and the MCP tools. `Save` always keeps the stored Enabled: only the lifecycle actions change it.
- web/node_modules is installed by the Linux dev container: on the macOS host `npm run build` fails (rolldown native binding) and NODE_ENV=production omits dev deps; build a copy in the scratchpad with `npm ci --include=dev` instead.
- Versioning (Mike's choice, 2026-11-18): version = `1.<git rev-list --count HEAD>` on master, computed in the CI image job (needs `fetch-depth: 0`). It tags the image (plus `latest` and `sha-<short>`) and is injected with `-X selfhoster/internal/buildinfo.version` through the Dockerfile VERSION build arg. .git is in .dockerignore, so Go's vcs info is empty in Docker builds; local builds fall back to the short commit, else "dev". Settings > System shows it.
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
- `GET /` is the UI catch-all; `<METHOD> /api/` routes (one per method) return a JSON 404. A method-less "/api/" pattern panics: it conflicts with "GET /" in Go's ServeMux.
- Tests must not depend on the UI being built (CI runs Go tests without it). Shared git test helpers live in `internal/testutil`.
- Git output can contain the repo URL (with a token): pass it through `redact` before surfacing it.
- GitHub repo: github.com/mystikxkemix/self-hoster (personal account). Pushes go over SSH with the personal key (repo-local `core.sshCommand` uses ~/.ssh/github_perso.pub); `gh` is logged in as the work account madmike75 and cannot see this repo. The old madmike75/self-hoster copy is obsolete.
- CI (.github/workflows/ci.yml) pins Go 1.27 / Node 24: keep in sync with the Dockerfile and dev container.
- zsh in this environment does not word-split `$VAR` in commands: use functions for curl helpers in tests.
