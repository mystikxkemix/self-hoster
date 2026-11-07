import type { ProjectStatus } from '../lib/types';

export function StatusBadge({ status }: { status: ProjectStatus }) {
  const label = status.deploying ? 'deploying' : status.state;
  return (
    <span className={`badge badge-${label}`}>
      <span className="dot" />
      {label}
    </span>
  );
}

/** Formats the time elapsed since an ISO date, e.g. "3h 12m". */
export function uptime(since?: string): string {
  if (!since) return '-';
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(since).getTime()) / 1000));
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${seconds}s`;
}
