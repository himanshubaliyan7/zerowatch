/**
 * Rate / Errors / Duration per service, from the ZeroWatch/RED namespace that
 * both the probes and the synthetic generator write (dimensions Service, Route).
 *
 * Storage matches a query's dimensions exactly (CloudWatch semantics), so a
 * per-service query must name Route as a wildcard.
 */
import { endpoints, RED_NAMESPACE, type AlarmRule, type MetricDataQuery, type QueryResult } from "./api";
import type { Tone } from "@/design/tokens";

export const WINDOW_MIN = 15;
export const MAX_SERVICES = 20;
/** A service is "Degraded" from this 5xx share, even without an alarm. */
export const DEGRADED_ERROR_PCT = 2;

export interface ServiceRed {
  name: string;
  rps: number | null;
  errorPct: number | null;
  p99: number | null;
  /** Per-minute series over the window, for sparklines and the detail board. */
  minutes: { t: number[]; req: (number | null)[]; err: (number | null)[]; p99: (number | null)[] };
  tone: Tone;
  status: string;
  alarms: string[];
}

export interface RedSnapshot {
  services: ServiceRed[];
  totals: { rps: number | null; errorPct: number | null; p50: number | null; p99: number | null };
  windowStart: number;
  windowEnd: number;
}

const dims = (service: string) => ({ Service: service, Route: "*" });
const stat = (id: string, metricName: string, s: string, d: Record<string, string>): MetricDataQuery => ({
  id,
  metricStat: { namespace: RED_NAMESPACE, metricName, stat: s, dimensions: d },
});

export async function listServices(): Promise<string[]> {
  const series = await endpoints.series(RED_NAMESPACE, "Requests");
  const names = new Set<string>();
  for (const s of series) if (s.dimensions?.Service) names.add(s.dimensions.Service);
  return [...names].sort().slice(0, MAX_SERVICES);
}

const sum = (v: number[]) => v.reduce((a, b) => a + b, 0);

/**
 * Align a result to the minute grid of the window; missing minutes stay null.
 * shiftMs moves the result's timestamps first (e.g. +7 days to overlay last week).
 */
export function onGrid(r: QueryResult | undefined, grid: number[], shiftMs = 0): (number | null)[] {
  if (!r) return grid.map(() => null);
  const byT = new Map<number, number>();
  r.timestamps.forEach((t, i) => byT.set(Math.floor((t + shiftMs) / 60_000) * 60_000, r.values[i]));
  return grid.map((t) => byT.get(t) ?? null);
}

export async function fetchRed(services: string[], rules: AlarmRule[], signal?: AbortSignal): Promise<RedSnapshot> {
  // Complete minutes only: a minute still filling would plot as a false drop in traffic.
  const windowEnd = Math.floor(Date.now() / 60_000) * 60_000;
  const windowStart = windowEnd - WINDOW_MIN * 60_000;
  const grid = Array.from({ length: WINDOW_MIN }, (_, i) => windowStart + i * 60_000);
  const all = { Service: "*", Route: "*" };

  const perMinute: MetricDataQuery[] = services.flatMap((s, i) => [
    stat(`r${i}`, "Requests", "Sum", dims(s)),
    stat(`e${i}`, "Errors", "Sum", dims(s)),
    stat(`l${i}`, "Latency", "p99", dims(s)),
  ]);
  const whole: MetricDataQuery[] = [
    stat("tr", "Requests", "Sum", all),
    stat("te", "Errors", "Sum", all),
    stat("tp50", "Latency", "p50", all),
    stat("tp99", "Latency", "p99", all),
    ...services.map((s, i) => stat(`w${i}`, "Latency", "p99", dims(s))),
  ];
  const span = WINDOW_MIN * 60;
  const [minute, window] = await Promise.all([
    perMinute.length
      ? endpoints.query({ queries: perMinute, start: windowStart, end: windowEnd, period: 60 }, signal)
      : Promise.resolve([] as QueryResult[]),
    endpoints.query({ queries: whole, start: windowStart, end: windowEnd, period: span }, signal),
  ]);
  const byId = new Map<string, QueryResult>();
  for (const r of [...minute, ...window]) byId.set(r.id, r);
  const total = (id: string) => {
    const r = byId.get(id);
    return r && r.values.length ? sum(r.values) : null;
  };
  const last = (id: string) => {
    const r = byId.get(id);
    return r && r.values.length ? r.values[r.values.length - 1] : null;
  };

  const firing = new Map<string, string[]>();
  for (const rule of rules) {
    const svc = rule.metric?.dimensions?.Service;
    if (svc && rule.state === "ALARM") firing.set(svc, [...(firing.get(svc) ?? []), rule.name]);
  }

  const out: ServiceRed[] = services.map((name, i) => {
    const req = byId.get(`r${i}`);
    const reqTotal = req && req.values.length ? sum(req.values) : 0;
    const errTotal = total(`e${i}`) ?? 0;
    const rps = reqTotal > 0 ? reqTotal / span : null;
    const errorPct = reqTotal > 0 ? (100 * errTotal) / reqTotal : null;
    const p99 = last(`w${i}`);
    const alarms = firing.get(name) ?? [];
    let tone: Tone = "positive";
    let status = "OK";
    if (alarms.length) {
      tone = "critical";
      status = "In alarm";
    } else if (rps == null) {
      tone = "unknown";
      status = "No data";
    } else if ((errorPct ?? 0) >= DEGRADED_ERROR_PCT) {
      tone = "warning";
      status = "Degraded";
    }
    return {
      name,
      rps,
      errorPct,
      p99,
      minutes: { t: grid, req: onGrid(req, grid), err: onGrid(byId.get(`e${i}`), grid), p99: onGrid(byId.get(`l${i}`), grid) },
      tone,
      status,
      alarms,
    };
  });

  const tr = total("tr");
  const te = total("te");
  return {
    services: out,
    totals: {
      rps: tr != null ? tr / span : null,
      errorPct: tr ? (100 * (te ?? 0)) / tr : null,
      p50: last("tp50"),
      p99: last("tp99"),
    },
    windowStart,
    windowEnd,
  };
}
