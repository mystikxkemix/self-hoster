// Mirrors the Go API (internal/store.Project and internal/supervisor.Status).

export type ProjectType = 'service' | 'static';
export type ProjectState = 'stopped' | 'running' | 'crashed';
export type ProjectAction = 'deploy' | 'start' | 'stop' | 'restart';

export interface ProjectStatus {
  state: ProjectState;
  pid?: number;
  startedAt?: string;
  restarts: number;
  deploying: boolean;
  commit?: string;
  lastError?: string;
}

/** The editable definition of a project, as accepted by POST/PUT. */
export interface ProjectSpec {
  name: string;
  type: ProjectType;
  repo: string;
  branch?: string;
  buildCmd?: string;
  startCmd?: string;
  port: number;
  env?: Record<string, string>;
  staticDir?: string;
  spa?: boolean;
  /** Public URL (e.g. served by nginx), used by the Open button. Optional. */
  url?: string;
  /** Redeploy automatically when a new commit lands on the branch (checked every minute). */
  autoDeploy?: boolean;
}

export interface Project extends ProjectSpec {
  enabled: boolean;
  /** Persistent folder for the project's data (SQLite files, uploads…), passed as DATA_DIR. Kept on delete. */
  dataDir: string;
  status: ProjectStatus;
}

/** Keeps only the fields the API accepts (it rejects unknown fields). */
export function toSpec(p: ProjectSpec): ProjectSpec {
  return {
    name: p.name,
    type: p.type,
    repo: p.repo,
    branch: p.branch,
    buildCmd: p.buildCmd,
    startCmd: p.type === 'service' ? p.startCmd : undefined,
    port: p.port,
    env: p.env,
    staticDir: p.type === 'static' ? p.staticDir : undefined,
    spa: p.type === 'static' ? p.spa : undefined,
    autoDeploy: p.autoDeploy,
    url: p.url,
  };
}

/** Admin settings, saved in settings.json on the data volume (mirrors internal/settings.View). */
export interface Settings {
  /** Running build: "1.42" from CI, a short commit locally, or "dev". */
  version: string;
  adminUser: string;
  /** "env" until the password is changed from the settings page. */
  passwordSource: 'env' | 'settings';
  pollInterval: string;
  pollIntervalDefault: string;
  /** Host used to open projects without a public URL ("" = the host of this page). */
  linkHost: string;
  info: { dataDir: string; appsDir: string; ports: string };
}

/** An MCP client (e.g. Claude) authorized through OAuth. */
export interface McpConnection {
  id: string;
  clientName: string;
  createdAt: string;
  lastUsed: string;
}

export interface McpStatus {
  /** False unless SELFHOSTER_MCP_ADDR is set. */
  enabled: boolean;
  /** Public MCP endpoint from SELFHOSTER_MCP_URL ("" when not set). */
  url: string;
  connections: McpConnection[];
}

export interface SettingsUpdate {
  adminUser: string;
  pollInterval: string;
  linkHost: string;
}
