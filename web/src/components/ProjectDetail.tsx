import { useState } from 'react';
import { api, errorMessage } from '../lib/api';
import type { Project, ProjectAction, ProjectSpec } from '../lib/types';
import { LogsView } from './LogsView';
import { ProjectForm } from './ProjectForm';
import { StatusBadge, uptime } from './StatusBadge';

type Tab = 'logs' | 'settings';

interface Props {
  project: Project;
  onChanged: () => void;
  onDeleted: () => void;
}

export function ProjectDetail({ project, onChanged, onDeleted }: Props) {
  const [tab, setTab] = useState<Tab>('logs');
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const { status } = project;
  const stopped = status.state === 'stopped';

  const run = async (label: string, fn: () => Promise<unknown>) => {
    setBusy(label);
    setError(null);
    try {
      await fn();
      onChanged();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const action = (a: ProjectAction) => {
    if (a === 'deploy') setTab('logs');
    return run(a, () => api.action(project.name, a));
  };

  const remove = () => {
    if (!confirm(`Delete ${project.name}? Its checkout is removed; the repository is untouched.`)) return;
    run('delete', async () => {
      await api.remove(project.name);
      onDeleted();
    });
  };

  const save = async (spec: ProjectSpec) => {
    await api.update(project.name, spec);
    onChanged();
  };

  return (
    <div className="detail">
      <header className="detail-header">
        <div>
          <h2>
            {project.name}
            <span className="tag">{project.type === 'static' ? 'website' : 'service'}</span>
            {project.autoDeploy && <span className="tag">auto-deploy</span>}
          </h2>
          <div className="meta">
            <StatusBadge status={status} />
            <span>
              port <strong>{project.port}</strong>
            </span>
            <span>commit <code>{status.commit ?? '-'}</code></span>
            <span>up {uptime(status.startedAt)}</span>
            {status.restarts > 0 && <span>{status.restarts} restarts</span>}
          </div>
        </div>
        <div className="actions">
          <button className="primary" disabled={!!busy || status.deploying} onClick={() => action('deploy')}>
            {status.deploying ? 'Deploying…' : 'Deploy'}
          </button>
          {stopped ? (
            <button disabled={!!busy} onClick={() => action('start')}>
              Start
            </button>
          ) : (
            <>
              <button disabled={!!busy} onClick={() => action('restart')}>
                Restart
              </button>
              <button disabled={!!busy} onClick={() => action('stop')}>
                Stop
              </button>
            </>
          )}
          <button className="danger" disabled={!!busy || status.deploying} onClick={remove}>
            Delete
          </button>
        </div>
      </header>

      {(error || status.lastError) && <div className="banner">{error ?? status.lastError}</div>}

      <nav className="tabs">
        <button className={tab === 'logs' ? 'active' : ''} onClick={() => setTab('logs')}>
          Logs
        </button>
        <button className={tab === 'settings' ? 'active' : ''} onClick={() => setTab('settings')}>
          Settings
        </button>
      </nav>

      {tab === 'logs' ? (
        <LogsView key={project.name} name={project.name} />
      ) : (
        <ProjectForm key={project.name} initial={project} submitLabel="Save changes" onSubmit={save} />
      )}
    </div>
  );
}
