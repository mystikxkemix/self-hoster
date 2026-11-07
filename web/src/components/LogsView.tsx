import { useLayoutEffect, useRef, useState } from 'react';
import { api, errorMessage } from '../lib/api';
import { usePolling } from '../lib/usePolling';

/** Live tail of a project's build and runtime output. Follows the end unless the user scrolls up. */
export function LogsView({ name }: { name: string }) {
  const [lines, setLines] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const pre = useRef<HTMLPreElement>(null);
  const follow = useRef(true);

  usePolling(async () => {
    try {
      setLines(await api.logs(name));
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    }
  }, 2000);

  useLayoutEffect(() => {
    if (follow.current && pre.current) pre.current.scrollTop = pre.current.scrollHeight;
  }, [lines]);

  const onScroll = () => {
    const el = pre.current;
    if (el) follow.current = el.scrollTop + el.clientHeight >= el.scrollHeight - 16;
  };

  return (
    <div className="logs">
      {error && <div className="error">{error}</div>}
      <pre ref={pre} onScroll={onScroll}>
        {lines.length === 0 ? 'No output yet. Deploy the project to see build logs here.' : lines.join('\n')}
      </pre>
    </div>
  );
}
