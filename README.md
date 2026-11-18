# self-hoster

Host personal projects from **one Docker container** on ZimaOS:

- **Services**: APIs and servers (Node/TypeScript, Go, anything with a build + start command).
- **Websites**: static or single-page apps (React, Vue, Astro…), served from their build folder.

A small Go supervisor clones each project's git repo, builds it, runs it on its own
port, injects its environment variables, and restarts it if it crashes. A web admin
(React) lets you register projects, edit env vars, deploy and follow logs. Routing
(domains, TLS) stays in your own nginx; databases (MongoDB…) stay yours too, you just
pass their connection string as an env var.

```
ZimaOS
└─ self-hoster container
   ├─ :8080        admin UI / API (basic auth)
   ├─ :4000 → my-api      (service: node dist/index.js)
   ├─ :4001 → my-go-svc   (service: ./bin/server)
   └─ :4002 → my-site     (website: serves dist/, no process)
   /data: config.json, project checkouts, build caches
   /apps: one persistent folder per project (/apps/my-api = its DATA_DIR)
```

## Install on ZimaOS

CI publishes `ghcr.io/mystikxkemix/self-hoster` (linux/amd64) after the tests pass on
`master`. Each push gets a version `1.<number of commits on master>` (e.g. `1.42`): the
image is tagged with it and with `latest`, and the Settings page shows it. To stay on one
version, use `:1.42` instead of `:latest`. The package is private:

1. Create a GitHub **classic** token with only `read:packages`, then on ZimaOS log in
   as your user (ZimaOS's root filesystem is read-only, so root cannot store a login)
   and pull with root pointing at your config:
   ```sh
   read -s GHCR_TOKEN && echo "$GHCR_TOKEN" | docker login ghcr.io -u mystikxkemix --password-stdin; unset GHCR_TOKEN
   sudo docker --config /DATA/.docker pull ghcr.io/mystikxkemix/self-hoster:latest
   ```
   (Your GitHub password does not work there: `denied: denied`.)
2. ZimaOS App Store → **+** (custom install) → **Import**, paste:
   ```yaml
   name: self-hoster
   services:
     self-hoster:
       image: ghcr.io/mystikxkemix/self-hoster:latest
       container_name: self-hoster
       restart: unless-stopped
       stop_grace_period: 30s
       # The ZimaOS importer rejects port ranges ("Invalid ports: 4000-4019"), so the
       # container uses the host network: admin and projects listen on the box directly,
       # and projects reach host services (e.g. MongoDB) at localhost.
       network_mode: host
       environment:
         SELFHOSTER_ADMIN_PASSWORD: change-me   # only until changed in Settings
         SELFHOSTER_ADDR: ":8090"               # admin UI (keep LAN-only)
         SELFHOSTER_PORTS: "4000-4019"          # projects, routed by your nginx
         # Optional, lets Claude manage projects (see "Claude (MCP)" below):
         # SELFHOSTER_MCP_URL: "https://selfhoster.example.com"
       volumes:
         - /DATA/AppData/self-hoster/data:/data   # settings, project list, checkouts
         - /DATA/AppData/self-hoster/apps:/apps   # each project's DATA_DIR (back up)
   ```
   Then open `http://<zima-ip>:8090`.
3. Update later: pull again with the `--config` command above, then
   `sudo docker compose up -d` in the app folder (or restart the ZimaOS app). To go back
   to an older version, put its tag (e.g. `:1.41`) in the compose `image:` line.

To build from source instead (any Docker host):

```sh
git clone <this repo> self-hoster && cd self-hoster
cp .env.example .env            # set SELFHOSTER_ADMIN_PASSWORD
docker compose up -d --build
```

Open `http://<zima-ip>:8080` (user `admin`). Keep this port on your LAN: the admin
UI can run arbitrary commands.

### Settings page

The admin UI has a **Settings** page (bottom of the sidebar). Changes are saved in
`data/settings.json` (the `/data` volume) and take precedence over the env vars below,
which act as defaults:

- **Admin username and password.** Once changed there, the password is stored hashed
  (PBKDF2) and `SELFHOSTER_ADMIN_PASSWORD` is no longer needed. Forgot it? Remove
  `"passwordHash"` from `data/settings.json` and restart: the env password applies again.
- **Auto-deploy check interval** (applies within a second, no restart; `0` disables).
- **Host for project links**: the **Open site** button opens a project's *Public URL*
  (set per project, e.g. the domain your nginx serves) or `http://<this host>:<port>`.

> Building on a Mac for the Zima box? ZimaOS is x86: `docker buildx build --platform linux/amd64 -t self-hoster .`

## Register a project

| Field | Service: Node / TS | Service: Go | Website (Vite, CRA…) |
|---|---|---|---|
| Build command | `npm ci && npm run build` | `go build -o bin/server .` | `npm ci && npm run build` |
| Start command | `node dist/index.js` | `./bin/server` | n/a |
| Folder to serve | n/a | n/a | `dist` (+ "Single-page app" for client routing) |

- **Services must listen on `$PORT`.** It is always set to the project's port.
- **Persistent data goes in `$DATA_DIR`** (`/apps/<name>`): SQLite files, uploads…
  The checkout is replaced on every deploy and deleted with the project; `/apps/<name>`
  is kept in both cases. Example: `DATABASE_PATH=/apps/budget-app/budget.db`.
  Back up the `apps/` folder.
- **Websites** are served by self-hoster itself (no extra process). Env vars are
  available at build time, e.g. `VITE_API_URL` for Vite.
- **Ports** must be in the published range (`4000-4019` by default). To widen it,
  change both `SELFHOSTER_PORTS` and the `ports:` mapping in `docker-compose.yml`.
- **Env vars** are edited in the UI as `KEY=value` lines. Saving restarts a running
  project so the change applies at once.
- **Deploy** fetches the branch, builds, then swaps the process. If the build fails,
  the previous version keeps running.
- **Auto-deploy** (on by default for new projects): self-hoster checks each running
  project's branch every minute (`git ls-remote`) and deploys new commits. Nothing has
  to reach the box: no webhook, no open port, no GitHub runner. A commit whose build
  failed is not retried; the next push is. Stopped projects are never auto-deployed.
- pnpm / yarn: use `corepack enable && pnpm install --frozen-lockfile && pnpm build`.

### Private repositories

- HTTPS: put a fine-grained read-only token in the URL,
  `https://<token>@github.com/me/repo.git`. It is stored in `data/config.json` (mode 600).
- SSH: mount a deploy key (uncomment the `./ssh:/root/.ssh:ro` volume) and use
  `git@github.com:me/repo.git`. Include a `known_hosts` file.

## nginx

One `server` block per project, pointing to the Zima box and the project's port:

```nginx
server {
    server_name api.example.com;
    location / {
        proxy_pass http://<zima-ip>:4000;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;   # websockets
        proxy_set_header Connection "upgrade";
    }
}
```

## Claude (MCP)

self-hoster can expose an [MCP](https://modelcontextprotocol.io) endpoint so Claude
(claude.ai, Desktop, mobile, Claude Code) can list projects, read logs, deploy,
start/stop/restart, create projects and edit their settings and env vars. Deleting a
project stays a web UI action. It is off by default.

The MCP endpoint is served by the admin server, on the admin port. Its routes use
OAuth; the admin UI and API keep their basic auth.

1. Set `SELFHOSTER_MCP_URL` on the container: the public HTTPS address nginx serves
   it at. This turns MCP on.
2. Expose it through nginx, over HTTPS. claude.ai connectors call it from Anthropic's
   cloud, so it must be reachable from the internet. **Forward only the MCP paths**:
   the admin UI runs on the same port and must stay LAN-only.
   ```nginx
   server {
       server_name selfhoster.example.com;   # = SELFHOSTER_MCP_URL
       # MCP endpoint and OAuth routes only. Everything else (admin UI, /api) is refused.
       location ~ ^/(mcp|authorize|token|register|\.well-known/oauth-[a-z-]+(/mcp)?)$ {
           proxy_pass http://<zima-ip>:8090;   # = SELFHOSTER_ADDR
           proxy_set_header Host $host;
           proxy_set_header X-Forwarded-Proto $scheme;
       }
       location / {
           return 404;
       }
   }
   ```
3. In Claude: **Settings → Connectors → Add custom connector**, URL
   `https://selfhoster.example.com/mcp`. With Claude Code:
   `claude mcp add --transport http self-hoster https://selfhoster.example.com/mcp`.
4. Claude opens a self-hoster page asking for the **admin username and password**.
   Allowing it connects Claude; access tokens last 1 hour and refresh for 30 days of
   inactivity.

Connected clients are listed on the **Settings** page, where each can be revoked at
once. Tokens are stored hashed in `data/oauth.json`; deleting that file disconnects
every client. The consent page locks for 30s (doubling, up to 15 min) after 5 wrong
passwords. Claude never sees env var values (only their names) nor tokens embedded in
repo URLs.

> Anyone holding a connection can change a project's build command, which runs as a
> shell command on the box. Use a strong admin password before exposing the endpoint.

## MongoDB

Not managed here: add e.g. `MONGO_URL=mongodb://my-api:<pw>@<zima-ip>:27017/my-api`
to the project's env. Recommended: one database + user per project.
Check your CPU first: MongoDB 5+ requires AVX (`grep -o -m1 avx /proc/cpuinfo`);
the original ZimaBoard (Celeron N3350/N3450) has none and is stuck on `mongo:4.4`.

## Settings

| Variable | Default | |
|---|---|---|
| `SELFHOSTER_ADMIN_PASSWORD` | (required until set in Settings) | Admin UI password |
| `SELFHOSTER_ADMIN_USER` | `admin` | Admin UI user (default; editable in Settings) |
| `SELFHOSTER_PORTS` | (any) | Allowed project port range, e.g. `4000-4019` |
| `SELFHOSTER_ADDR` | `:8080` | Admin UI listen address |
| `SELFHOSTER_DATA_DIR` | `/data` | State, checkouts and caches |
| `SELFHOSTER_APPS_DIR` | `/apps` | Parent of each project's persistent `DATA_DIR` |
| `SELFHOSTER_POLL_INTERVAL` | `1m` | Auto-deploy check interval (default; editable in Settings) |
| `SELFHOSTER_MCP_URL` | (off) | Public HTTPS base URL of the MCP endpoint, e.g. `https://selfhoster.example.com`. Setting it turns MCP on |

`SELFHOSTER_*` variables are never passed to projects.

## API

All routes use basic auth. `GET/POST /api/projects`,
`GET/PUT/DELETE /api/projects/{name}`, `POST /api/projects/{name}/{deploy|start|stop|restart}`,
`GET /api/projects/{name}/logs`, `GET /api/mcp`, `DELETE /api/mcp/connections/{id}`. When MCP is on, `/mcp`, `/authorize`,
`/token`, `/register` and `/.well-known/oauth-*` use OAuth instead of basic auth. Unknown `/api` routes return a JSON 404.

Auto-deploy usually makes manual calls unnecessary, but a deploy can be forced with:

```sh
curl -fsS -u admin:$PASSWORD -X POST http://<zima-ip>:8080/api/projects/my-api/deploy
```

## Trade-offs (by design)

- Updating self-hoster restarts every project.
- Projects share the container: no CPU/RAM isolation between them.
- Builds run on the Zima box: slower than CI, but no registry or pipeline needed.
- Env vars are stored in plain JSON on the data volume (mode 600). Protect the volume.

## Development (dev container)

Open the folder in VS Code and choose **Reopen in Container** (or
`devcontainer up --workspace-folder .`). The container has Go 1.27 and Node 24, the same
as the runtime image, and the env vars already set (admin password: `dev`).

```sh
go run ./cmd/self-hoster          # API + embedded UI on :8080
cd web && npm run dev             # admin UI with hot reload on :5173 (proxies /api to :8080)
```

- `web/` is the React + Vite + TypeScript admin. `npm run build` writes to
  `internal/api/ui/dist`, which is embedded in the Go binary (git-ignored).
- Without a web build, the binary serves a placeholder page; the API still works.
- Project state lives in `./data` (git-ignored). Ports 4000-4019 are forwarded, so
  hosted projects are reachable from your host like on ZimaOS.
