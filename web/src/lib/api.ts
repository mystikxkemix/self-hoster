import { type Project, type ProjectAction, type ProjectSpec, toSpec } from './types';

const BASE = '/api/projects';

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  // Absolute URL from origin: a relative fetch throws when the page was opened as http://user:pass@host.
  const res = await fetch(new URL(BASE + path, window.location.origin), {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error ?? `${res.status} ${res.statusText}`);
  return data as T;
}

const path = (name: string) => `/${encodeURIComponent(name)}`;

export const api = {
  list: () => request<Project[]>('GET', ''),
  create: (spec: ProjectSpec) => request<Project>('POST', '', toSpec(spec)),
  update: (name: string, spec: ProjectSpec) => request<Project>('PUT', path(name), toSpec(spec)),
  remove: (name: string) => request<void>('DELETE', path(name)),
  action: (name: string, action: ProjectAction) => request<Project>('POST', `${path(name)}/${action}`),
  logs: (name: string) => request<string[]>('GET', `${path(name)}/logs`),
};

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
