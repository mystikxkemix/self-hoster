import { type FormEvent, useState } from 'react';
import { api, errorMessage } from '../lib/api';
import type { Settings } from '../lib/types';

interface Props {
  settings: Settings;
  onSaved: (settings: Settings) => void;
}

/** Reloads the page so the browser asks for the new credentials. */
function signInAgain() {
  setTimeout(() => window.location.reload(), 1500);
}

export function SettingsPage({ settings, onSaved }: Props) {
  return (
    <div className="detail">
      <header className="detail-header">
        <div>
          <h2>Settings</h2>
          <p className="hint">
            Saved in <code>{settings.info.dataDir}/settings.json</code>, on the data volume. They override the
            SELFHOSTER_* environment variables.
          </p>
        </div>
      </header>
      <GeneralSettings settings={settings} onSaved={onSaved} />
      <PasswordSettings settings={settings} onSaved={onSaved} />
      <SystemInfo settings={settings} />
    </div>
  );
}

function GeneralSettings({ settings, onSaved }: Props) {
  const [adminUser, setAdminUser] = useState(settings.adminUser);
  const [pollInterval, setPollInterval] = useState(settings.pollInterval);
  const [linkHost, setLinkHost] = useState(settings.linkHost);
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  const [saving, setSaving] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setSaving(true);
    setStatus(null);
    try {
      const saved = await api.updateSettings({ adminUser, pollInterval, linkHost });
      onSaved(saved);
      if (saved.adminUser !== settings.adminUser) {
        setStatus({ ok: true, text: 'Saved. Sign in again with the new username…' });
        signInAgain();
      } else {
        setStatus({ ok: true, text: 'Saved.' });
      }
    } catch (err) {
      setStatus({ ok: false, text: errorMessage(err) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <form className="form" onSubmit={submit}>
      <section>
        <h3>Admin</h3>
        <div className="grid">
          <label>
            Username
            <input required value={adminUser} onChange={(e) => setAdminUser(e.target.value)} autoComplete="username" />
          </label>
        </div>
      </section>
      <section>
        <h3>Auto-deploy</h3>
        <div className="grid">
          <label>
            Check interval
            <input
              className="mono"
              value={pollInterval}
              placeholder={settings.pollIntervalDefault}
              onChange={(e) => setPollInterval(e.target.value)}
            />
            <span className="hint">
              How often repositories are checked for new commits: 30s, 1m, 5m… 0 disables auto-deploy. Empty uses
              the default ({settings.pollIntervalDefault}).
            </span>
          </label>
        </div>
      </section>
      <section>
        <h3>Links</h3>
        <div className="grid">
          <label>
            Host for project links
            <input
              className="mono"
              value={linkHost}
              placeholder={window.location.hostname}
              onChange={(e) => setLinkHost(e.target.value)}
            />
            <span className="hint">
              The Open button goes to a project's public URL, or to http://this-host:port. Empty uses the host of
              this page.
            </span>
          </label>
        </div>
      </section>
      <div className="form-actions">
        <button type="submit" className="primary" disabled={saving}>
          {saving ? 'Saving…' : 'Save settings'}
        </button>
        {status && <span className={status.ok ? 'success' : 'error'}>{status.text}</span>}
      </div>
    </form>
  );
}

function PasswordSettings({ settings }: Props) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  const [saving, setSaving] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (next !== confirmation) {
      setStatus({ ok: false, text: 'The new passwords do not match.' });
      return;
    }
    setSaving(true);
    setStatus(null);
    try {
      await api.changePassword(current, next);
      setStatus({ ok: true, text: 'Password changed. Sign in again…' });
      signInAgain();
    } catch (err) {
      setStatus({ ok: false, text: errorMessage(err) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <form className="form" onSubmit={submit}>
      <section>
        <h3>Password</h3>
        <p className="hint section-hint">
          {settings.passwordSource === 'env'
            ? 'Currently the SELFHOSTER_ADMIN_PASSWORD environment variable. Once changed here, it is stored hashed and the variable is no longer used.'
            : 'Set from this page and stored hashed. To reset a forgotten password, remove "passwordHash" from settings.json: SELFHOSTER_ADMIN_PASSWORD applies again.'}
        </p>
        <div className="grid">
          <label className="full">
            Current password
            <input
              type="password"
              required
              value={current}
              autoComplete="current-password"
              onChange={(e) => setCurrent(e.target.value)}
            />
          </label>
          <label>
            New password
            <input
              type="password"
              required
              minLength={8}
              value={next}
              autoComplete="new-password"
              onChange={(e) => setNext(e.target.value)}
            />
          </label>
          <label>
            Confirm new password
            <input
              type="password"
              required
              minLength={8}
              value={confirmation}
              autoComplete="new-password"
              onChange={(e) => setConfirmation(e.target.value)}
            />
          </label>
        </div>
      </section>
      <div className="form-actions">
        <button type="submit" className="primary" disabled={saving}>
          {saving ? 'Changing…' : 'Change password'}
        </button>
        {status && <span className={status.ok ? 'success' : 'error'}>{status.text}</span>}
      </div>
    </form>
  );
}

function SystemInfo({ settings }: { settings: Settings }) {
  const rows: [string, string][] = [
    ['Data folder (state, checkouts, caches)', settings.info.dataDir],
    ['Projects data folders (DATA_DIR)', settings.info.appsDir],
    ['Allowed project ports', settings.info.ports || 'any'],
  ];
  return (
    <section className="info">
      <h3>System</h3>
      <dl>
        {rows.map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>
              <code>{value}</code>
            </dd>
          </div>
        ))}
      </dl>
      <p className="hint">These come from the container environment and its volumes (docker-compose.yml).</p>
    </section>
  );
}
