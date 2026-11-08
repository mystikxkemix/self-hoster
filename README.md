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

CI publishes `ghcr.io/mystikxkemix/self-hoster:latest` (linux/amd64) after the tests
pass on `master`. The package is private:

1. Create a GitHub **classic** token with only `read:packages`, then on ZimaOS:
   ```sh
   read -s GHCR_TOKEN && echo "$GHCR_TOKEN" | sudo docker login ghcr.io -u mystikxkemix --password-stdin; unset GHCR_TOKEN
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
       environment:
         SELFHOSTER_ADMIN_PASSWORD: change-me   # only until changed in Settings
         SELFHOSTER_PORTS: "4000-4019"
       ports:
         - "8080:8080"             # admin UI (keep LAN-only)
         - "4000-4019:4000-4019"   # projects, routed by your nginx
       volumes:
         - /DATA/AppData/self-hoster/data:/data   # settings, project list, checkouts
         - /DATA/AppData/self-hoster/apps:/apps   # each project's DATA_DIR (back up)
   ```
3. Update later: `sudo docker compose pull && sudo docker compose up -d` in the app
   folder, or the update action of the ZimaOS app.

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

`SELFHOSTER_*` variables are never passed to projects.

## API

All routes use basic auth. `GET/POST /api/projects`,
`GET/PUT/DELETE /api/projects/{name}`, `POST /api/projects/{name}/{deploy|start|stop|restart}`,
`GET /api/projects/{name}/logs`. Unknown `/api` routes return a JSON 404.

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
