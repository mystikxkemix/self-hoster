import { type Project, type ProjectAction, type ProjectSpec, type Settings, type SettingsUpdate, toSpec } from './types';

const BASE = '/api';

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

const path = (name: string) => `/projects/${encodeURIComponent(name)}`;

export const api = {
  list: () => request<Project[]>('GET', '/projects'),
  create: (spec: ProjectSpec) => request<Project>('POST', '/projects', toSpec(spec)),
  update: (name: string, spec: ProjectSpec) => request<Project>('PUT', path(name), toSpec(spec)),
  remove: (name: string) => request<void>('DELETE', path(name)),
  action: (name: string, action: ProjectAction) => request<Project>('POST', `${path(name)}/${action}`),
  logs: (name: string) => request<string[]>('GET', `${path(name)}/logs`),
  settings: () => request<Settings>('GET', '/settings'),
  updateSettings: (update: SettingsUpdate) => request<Settings>('PUT', '/settings', update),
  changePassword: (current: string, next: string) =>
    request<void>('POST', '/settings/password', { current, new: next }),
};

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
