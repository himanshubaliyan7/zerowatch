import { useCallback, useEffect, useRef, useState } from "react";

export interface Polled<T> {
  data: T | undefined;
  error: Error | undefined;
  loading: boolean;
  refresh: () => void;
}

/**
 * Calls `fn` now and every `intervalMs` while the tab is visible. Keeps the
 * last good data on error, so a blip does not blank the screen.
 */
export function usePoll<T>(fn: (signal: AbortSignal) => Promise<T>, intervalMs: number, deps: readonly unknown[] = []): Polled<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<Error>();
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);
  const fnRef = useRef(fn);
  fnRef.current = fn;

  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let ctrl: AbortController | undefined;
    // A hidden tab waits, then refreshes the moment it is shown (not a whole interval later).
    const onVisible = () => {
      if (document.hidden || !alive) return;
      document.removeEventListener("visibilitychange", onVisible);
      if (timer) clearTimeout(timer);
      void run();
    };
    const run = async () => {
      if (document.hidden) {
        document.addEventListener("visibilitychange", onVisible);
        return;
      }
      ctrl = new AbortController();
      try {
        const v = await fnRef.current(ctrl.signal);
        if (!alive) return;
        setData(v);
        setError(undefined);
      } catch (e) {
        if (!alive || (e instanceof DOMException && e.name === "AbortError")) return;
        setError(e instanceof Error ? e : new Error(String(e)));
      } finally {
        if (alive) {
          setLoading(false);
          timer = setTimeout(run, intervalMs);
        }
      }
    };
    void run();
    return () => {
      alive = false;
      ctrl?.abort();
      if (timer) clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [intervalMs, nonce, ...deps]);

  const refresh = useCallback(() => setNonce((n) => n + 1), []);
  return { data, error, loading, refresh };
}

/** A clock that ticks every `ms`, for "updated 4 s ago" and redraws. */
export function useNow(ms = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}

export function prefersReducedMotion(): boolean {
  return typeof window.matchMedia !== "function" || window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

/**
 * A number that counts from the value shown before to the new one (quartic
 * ease). The first render already shows the final value when motion is off.
 */
export function useCountUp(target: number, durationMs = 1100): number {
  const [shown, setShown] = useState(() => (prefersReducedMotion() ? target : 0));
  const from = useRef(prefersReducedMotion() ? target : 0);

  useEffect(() => {
    const start = from.current;
    from.current = target;
    if (prefersReducedMotion() || start === target || !Number.isFinite(target)) {
      setShown(target);
      return;
    }
    let frame = 0;
    const t0 = performance.now();
    const step = (now: number) => {
      const p = Math.min(1, (now - t0) / durationMs);
      setShown(start + (target - start) * (1 - (1 - p) ** 4));
      if (p < 1) frame = requestAnimationFrame(step);
    };
    frame = requestAnimationFrame(step);
    return () => cancelAnimationFrame(frame);
  }, [target, durationMs]);

  return shown;
}

/** The current hash route: "#/logs?q=service%3Dcheckout" -> { view: "logs", params }. */
export function useRoute(): { view: string; params: URLSearchParams } {
  const read = () => {
    const h = window.location.hash.replace(/^#\/?/, "");
    const [view, qs] = h.split("?");
    return { view: view || "overview", params: new URLSearchParams(qs ?? "") };
  };
  const [route, setRoute] = useState(read);
  useEffect(() => {
    const on = () => setRoute(read());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  return route;
}

export function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}
