import { useEffect, useRef } from 'react';

/** Calls `fn` immediately, then every `intervalMs`, until unmount or `enabled` turns false. */
export function usePolling(fn: () => void | Promise<void>, intervalMs: number, enabled = true) {
  const latest = useRef(fn);
  latest.current = fn;

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      await latest.current();
      if (!cancelled) timer = setTimeout(tick, intervalMs);
    };
    tick();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [intervalMs, enabled]);
}
