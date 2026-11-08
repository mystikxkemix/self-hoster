import type { Project } from './types';

/**
 * Where to open a project: its public URL when set (e.g. behind nginx), otherwise
 * http://<link host>:<port>, the link host defaulting to the host of this admin page.
 */
export function projectUrl(project: Pick<Project, 'url' | 'port'>, linkHost: string): string {
  if (project.url) return project.url;
  const host = linkHost || window.location.hostname;
  // IPv6 literals need brackets in URLs.
  const hostPart = host.includes(':') ? `[${host}]` : host;
  return `http://${hostPart}:${project.port}`;
}
