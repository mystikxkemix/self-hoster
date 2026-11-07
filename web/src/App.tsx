import { useCallback, useState } from 'react';
import { ProjectDetail } from './components/ProjectDetail';
import { ProjectForm } from './components/ProjectForm';
import { StatusBadge } from './components/StatusBadge';
import { api, errorMessage } from './lib/api';
import type { Project, ProjectSpec } from './lib/types';
import { usePolling } from './lib/usePolling';

type View = { kind: 'project'; name: string } | { kind: 'create' } | { kind: 'none' };

export function App() {
  const [projects, setProjects] = useState<Project[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [view, setView] = useState<View>({ kind: 'none' });

  const refresh = useCallback(async () => {
    try {
      const list = await api.list();
      setProjects(list);
      setLoadError(null);
      // Select the first project by default.
      setView((v) => (v.kind === 'none' && list.length > 0 ? { kind: 'project', name: list[0].name } : v));
    } catch (err) {
      setLoadError(errorMessage(err));
    }
  }, []);

  usePolling(refresh, 3000);

  const create = async (spec: ProjectSpec) => {
    const created = await api.create(spec);
    await refresh();
    setView({ kind: 'project', name: created.name });
  };

  const selected = view.kind === 'project' ? projects?.find((p) => p.name === view.name) : undefined;

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">self-hoster</div>
        <button className="primary block" onClick={() => setView({ kind: 'create' })}>
          + New project
        </button>
        <ul className="project-list">
          {projects?.map((p) => (
            <li key={p.name}>
              <button
                className={selected?.name === p.name ? 'active' : ''}
                onClick={() => setView({ kind: 'project', name: p.name })}
              >
                <span className="name">{p.name}</span>
                <span className="sub">
                  <StatusBadge status={p.status} />
                  <span>:{p.port}</span>
                </span>
              </button>
            </li>
          ))}
        </ul>
        {projects?.length === 0 && <p className="hint">No projects yet.</p>}
      </aside>

      <main className="main">
        {loadError && <div className="banner">Cannot reach the API: {loadError}</div>}
        {view.kind === 'create' && (
          <div className="detail">
            <header className="detail-header">
              <h2>New project</h2>
            </header>
            <ProjectForm submitLabel="Create project" onSubmit={create} onCancel={() => setView({ kind: 'none' })} />
          </div>
        )}
        {selected && (
          <ProjectDetail project={selected} onChanged={refresh} onDeleted={() => setView({ kind: 'none' })} />
        )}
        {view.kind !== 'create' && !selected && projects && (
          <div className="empty">
            <h2>Host your projects</h2>
            <p>Register a git repository: an API or server (Node, Go…) or a website build folder.</p>
            <button className="primary" onClick={() => setView({ kind: 'create' })}>
              + New project
            </button>
          </div>
        )}
      </main>
    </div>
  );
}
