/** Time ranges shared by Explore and Dashboards: span and a sensible period. */
export const RANGES = [
  { id: "15m", label: "15 min", ms: 15 * 60_000, period: 60 },
  { id: "1h", label: "1 h", ms: 60 * 60_000, period: 60 },
  { id: "6h", label: "6 h", ms: 6 * 60 * 60_000, period: 300 },
  { id: "24h", label: "24 h", ms: 24 * 60 * 60_000, period: 900 },
] as const;

export type RangeId = (typeof RANGES)[number]["id"];

export function range(id: string | undefined) {
  return RANGES.find((r) => r.id === id) ?? RANGES[1];
}

/** [start, end) aligned to the period, ending at the last complete period. */
export function timeWindow(id: string | undefined, now = Date.now()) {
  const r = range(id);
  const p = r.period * 1000;
  const end = Math.floor(now / p) * p;
  return { start: end - r.ms, end, period: r.period };
}

/** Hash link to the Logs view for a time window and filter. */
export function logsLink(from: number, to: number, q = ""): string {
  const p = new URLSearchParams();
  if (q) p.set("q", q);
  p.set("from", String(Math.round(from)));
  p.set("to", String(Math.round(to)));
  return `#/logs?${p.toString()}`;
}
