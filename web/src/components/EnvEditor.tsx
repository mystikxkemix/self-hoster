import { useState } from 'react';

export interface EnvRow {
  id: number;
  key: string;
  value: string;
}

let nextId = 0;
const row = (key = '', value = ''): EnvRow => ({ id: nextId++, key, value });

export function toRows(env?: Record<string, string>): EnvRow[] {
  return Object.entries(env ?? {}).map(([k, v]) => row(k, v));
}

/** Converts rows back to a record, dropping blank keys. Throws on duplicates. */
export function toEnv(rows: EnvRow[]): Record<string, string> {
  const env: Record<string, string> = {};
  for (const r of rows) {
    const key = r.key.trim();
    if (!key) continue;
    if (key in env) throw new Error(`Duplicate environment variable ${key}`);
    env[key] = r.value;
  }
  return env;
}

/** Parses pasted .env content ("KEY=value" lines, # comments) into rows. */
function parseDotEnv(text: string): EnvRow[] {
  const rows: EnvRow[] = [];
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const i = line.indexOf('=');
    if (i <= 0) continue;
    let value = line.slice(i + 1).trim();
    if (/^(['"]).*\1$/.test(value)) value = value.slice(1, -1);
    rows.push(row(line.slice(0, i).replace(/^export\s+/, '').trim(), value));
  }
  return rows;
}

interface Props {
  rows: EnvRow[];
  onChange: (rows: EnvRow[]) => void;
}

export function EnvEditor({ rows, onChange }: Props) {
  const [revealed, setRevealed] = useState(false);
  const [pasting, setPasting] = useState(false);
  const [pasteText, setPasteText] = useState('');

  const update = (id: number, patch: Partial<EnvRow>) =>
    onChange(rows.map((r) => (r.id === id ? { ...r, ...patch } : r)));

  const importPasted = () => {
    const imported = parseDotEnv(pasteText);
    const byKey = new Map(rows.map((r) => [r.key, r]));
    for (const r of imported) byKey.set(r.key, r);
    onChange([...byKey.values()].filter((r) => r.key || r.value));
    setPasteText('');
    setPasting(false);
  };

  return (
    <div className="env">
      <div className="env-toolbar">
        <span className="hint">
          PORT and DATA_DIR (persistent folder, kept across deploys) are set automatically.
        </span>
        <button type="button" className="link" onClick={() => setRevealed(!revealed)}>
          {revealed ? 'Hide values' : 'Show values'}
        </button>
        <button type="button" className="link" onClick={() => setPasting(!pasting)}>
          Paste .env
        </button>
      </div>
      {pasting && (
        <div className="env-paste">
          <textarea
            value={pasteText}
            onChange={(e) => setPasteText(e.target.value)}
            placeholder={'MONGO_URL=mongodb://user:pass@192.168.1.10:27017/app\nAPI_KEY=...'}
            rows={5}
          />
          <button type="button" onClick={importPasted} disabled={!pasteText.trim()}>
            Import
          </button>
        </div>
      )}
      {rows.map((r) => (
        <div className="env-row" key={r.id}>
          <input
            className="mono"
            value={r.key}
            placeholder="KEY"
            onChange={(e) => update(r.id, { key: e.target.value })}
          />
          <input
            className="mono"
            type={revealed ? 'text' : 'password'}
            value={r.value}
            placeholder="value"
            autoComplete="off"
            onChange={(e) => update(r.id, { value: e.target.value })}
          />
          <button
            type="button"
            className="icon"
            aria-label={`Remove ${r.key || 'variable'}`}
            onClick={() => onChange(rows.filter((x) => x.id !== r.id))}
          >
            ×
          </button>
        </div>
      ))}
      <button type="button" className="link" onClick={() => onChange([...rows, row()])}>
        + Add variable
      </button>
    </div>
  );
}
