import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";

export interface HubEvent {
  type: "run" | "visit" | "task" | "flow" | "progress";
  id: string;
  seq?: number;
  text?: string;
}

type Listener = (e: HubEvent) => void;

// One shared EventSource for the whole app.
const listeners = new Set<Listener>();
let source: EventSource | null = null;
let connected = false;
const statusListeners = new Set<() => void>();

function setConnected(v: boolean) {
  if (connected !== v) {
    connected = v;
    statusListeners.forEach((l) => l());
  }
}

function ensureSource() {
  if (source) return;
  source = new EventSource("/api/events");
  source.onopen = () => setConnected(true);
  source.onerror = () => setConnected(false); // EventSource reconnects on its own
  source.onmessage = (m) => {
    try {
      const e = JSON.parse(m.data) as HubEvent;
      listeners.forEach((l) => l(e));
    } catch {
      /* ignore malformed */
    }
  };
}

/** Subscribe to server events for the lifetime of the component. */
export function useEvents(fn: Listener) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    ensureSource();
    const l: Listener = (e) => ref.current(e);
    listeners.add(l);
    return () => {
      listeners.delete(l);
    };
  }, []);
}

/** Whether the live event stream is connected. */
export function useLive(): boolean {
  ensureSource();
  return useSyncExternalStore(
    (cb) => {
      statusListeners.add(cb);
      return () => statusListeners.delete(cb);
    },
    () => connected,
  );
}

export interface Resource<T> {
  data: T | undefined;
  error: string | undefined;
  loading: boolean;
  reload: () => void;
}

/**
 * Load data and reload it (debounced) whenever `shouldReload` says an event is
 * relevant. Keeps showing the previous data while reloading.
 */
export function useResource<T>(load: () => Promise<T>, deps: unknown[], shouldReload?: (e: HubEvent) => boolean): Resource<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(true);
  const loadRef = useRef(load);
  loadRef.current = load;
  const seq = useRef(0);

  const reload = useCallback(() => {
    const mine = ++seq.current;
    loadRef
      .current()
      .then((d) => {
        if (mine === seq.current) {
          setData(d);
          setError(undefined);
        }
      })
      .catch((e: Error) => mine === seq.current && setError(e.message))
      .finally(() => mine === seq.current && setLoading(false));
  }, []);

  useEffect(() => {
    setLoading(true);
    setData(undefined);
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  const timer = useRef<number | undefined>(undefined);
  useEvents((e) => {
    if (shouldReload && shouldReload(e)) {
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(reload, 150);
    }
  });

  return { data, error, loading, reload };
}

/** Re-render every `ms` (for relative times and elapsed counters). */
export function useNow(ms = 1000): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), ms);
    return () => window.clearInterval(t);
  }, [ms]);
  return now;
}

export function useLocalStorage<T>(key: string, initial: T): [T, (v: T) => void] {
  const [v, setV] = useState<T>(() => {
    try {
      const raw = localStorage.getItem(key);
      return raw ? (JSON.parse(raw) as T) : initial;
    } catch {
      return initial;
    }
  });
  const set = useCallback(
    (nv: T) => {
      setV(nv);
      try {
        localStorage.setItem(key, JSON.stringify(nv));
      } catch {
        /* storage unavailable */
      }
    },
    [key],
  );
  return [v, set];
}
