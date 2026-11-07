import { type FormEvent, useState } from 'react';
import { errorMessage } from '../lib/api';
import type { ProjectSpec, ProjectType } from '../lib/types';
import { EnvEditor, toEnv, toRows } from './EnvEditor';

const PLACEHOLDERS: Record<ProjectType, { build: string; start?: string }> = {
  service: { build: 'npm ci && npm run build   ·   go build -o bin/server .', start: 'node dist/index.js   ·   ./bin/server' },
  static: { build: 'npm ci && npm run build' },
};

const EMPTY: ProjectSpec = { name: '', type: 'service', repo: '', port: 4000, spa: true, staticDir: 'dist', autoDeploy: true };

interface Props {
  initial?: ProjectSpec;
  submitLabel: string;
  onSubmit: (spec: ProjectSpec) => Promise<void>;
  onCancel?: () => void;
}

export function ProjectForm({ initial, submitLabel, onSubmit, onCancel }: Props) {
  const editing = !!initial;
  const [spec, setSpec] = useState<ProjectSpec>(initial ?? EMPTY);
  const [envRows, setEnvRows] = useState(() => toRows(initial?.env));
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const set = <K extends keyof ProjectSpec>(key: K, value: ProjectSpec[K]) => setSpec((s) => ({ ...s, [key]: value }));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setSaving(true);
    try {
      await onSubmit({ ...spec, env: toEnv(envRows) });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const placeholders = PLACEHOLDERS[spec.type];

  return (
    <form className="form" onSubmit={submit}>
      <section>
        <h3>Project</h3>
        <div className="grid">
          <label>
            Name
            <input
              required
              disabled={editing}
              pattern="[a-z0-9][a-z0-9\-]{0,62}"
              title="Lowercase letters, digits and dashes"
              value={spec.name}
              placeholder="my-api"
              onChange={(e) => set('name', e.target.value)}
            />
          </label>
          <label>
            Port
            <input
              required
              type="number"
              min={1}
              max={65535}
              value={spec.port || ''}
              onChange={(e) => set('port', Number(e.target.value))}
            />
          </label>
          <div className="full">
            <span className="label">Type</span>
            <div className="segmented" role="radiogroup">
              {(['service', 'static'] as const).map((t) => (
                <button
                  type="button"
                  key={t}
                  role="radio"
                  aria-checked={spec.type === t}
                  className={spec.type === t ? 'active' : ''}
                  onClick={() => set('type', t)}
                >
                  <strong>{t === 'service' ? 'Service' : 'Website'}</strong>
                  <span>{t === 'service' ? 'API or server process (Node, Go…)' : 'Static build folder (React, Vue, Astro…)'}</span>
                </button>
              ))}
            </div>
          </div>
        </div>
      </section>

      <section>
        <h3>Source</h3>
        <div className="grid">
          <label className="full">
            Git repository
            <input
              required
              className="mono"
              value={spec.repo}
              placeholder="https://github.com/me/my-api.git"
              onChange={(e) => set('repo', e.target.value)}
            />
          </label>
          <label>
            Branch
            <input
              className="mono"
              value={spec.branch ?? ''}
              placeholder="default branch"
              onChange={(e) => set('branch', e.target.value)}
            />
          </label>
          <label className="checkbox">
            <input type="checkbox" checked={!!spec.autoDeploy} onChange={(e) => set('autoDeploy', e.target.checked)} />
            Auto-deploy new commits (checked every minute)
          </label>
        </div>
      </section>

      <section>
        <h3>Build &amp; run</h3>
        <div className="grid">
          <label className="full">
            Build command
            <input
              className="mono"
              value={spec.buildCmd ?? ''}
              placeholder={placeholders.build}
              onChange={(e) => set('buildCmd', e.target.value)}
            />
          </label>
          {spec.type === 'service' ? (
            <label className="full">
              Start command
              <input
                required
                className="mono"
                value={spec.startCmd ?? ''}
                placeholder={placeholders.start}
                onChange={(e) => set('startCmd', e.target.value)}
              />
              <span className="hint">Your app must listen on $PORT.</span>
            </label>
          ) : (
            <>
              <label>
                Folder to serve
                <input
                  required
                  className="mono"
                  value={spec.staticDir ?? ''}
                  placeholder="dist"
                  onChange={(e) => set('staticDir', e.target.value)}
                />
              </label>
              <label className="checkbox">
                <input type="checkbox" checked={!!spec.spa} onChange={(e) => set('spa', e.target.checked)} />
                Single-page app (serve index.html for unknown routes)
              </label>
            </>
          )}
        </div>
      </section>

      <section>
        <h3>Environment</h3>
        <EnvEditor rows={envRows} onChange={setEnvRows} />
      </section>

      <div className="form-actions">
        <button type="submit" className="primary" disabled={saving}>
          {saving ? 'Saving…' : submitLabel}
        </button>
        {onCancel && (
          <button type="button" onClick={onCancel}>
            Cancel
          </button>
        )}
        {error && <span className="error">{error}</span>}
        {editing && !error && <span className="hint">Saving restarts the project if it is running.</span>}
      </div>
    </form>
  );
}
