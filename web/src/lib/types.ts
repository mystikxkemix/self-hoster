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
  /** Redeploy automatically when a new commit lands on the branch (checked every minute). */
  autoDeploy?: boolean;
}

export interface Project extends ProjectSpec {
  enabled: boolean;
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
  };
}
