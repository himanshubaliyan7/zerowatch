/**
 * Dashboard widget queries, read out of the loosely typed Widget.query, and
 * the same validation rules the server applies before it saves a board.
 */
import type { Dashboard, LogQuery, MetricDataQuery, Widget } from "@/lib/api";
import { range, timeWindow } from "@/lib/range";

export const GRID_COLUMNS = 12;
export const MAX_WIDGETS = 100;
export const MAX_Y = 40;
export const MAX_H = 8;
export const MIN_REFRESH = 10;
export const DEFAULT_REFRESH = 30;
export const PERIODS = [60, 300, 900, 3600] as const;

export const WIDGET_TYPES: { id: Widget["type"]; label: string }[] = [
  { id: "timeseries", label: "Time series" },
  { id: "stat", label: "Stat" },
  { id: "anomaly", label: "Anomaly band" },
  { id: "forecast", label: "Forecast" },
  { id: "logs", label: "Logs" },
  { id: "alarm", label: "Alarms" },
  { id: "text", label: "Text" },
];

export const typeLabel = (t: Widget["type"]) => WIDGET_TYPES.find((x) => x.id === t)?.label ?? t;

export const isMetricType = (t: Widget["type"]) => t === "timeseries" || t === "stat" || t === "anomaly" || t === "forecast";

/** An anomaly widget's band model (Holt-Winters unless the query says "stl"). */
export function anomalyMethod(w: Widget): "holtwinters" | "stl" {
  return obj(w.query).method === "stl" ? "stl" : "holtwinters";
}

/** Forecast horizons a widget offers, in minutes. */
export const FORECAST_HORIZONS = [60, 360, 1440, 4320] as const;

/** A forecast widget's horizon (minutes, default 6 h) and optional threshold. */
export function forecastOf(w: Widget): { horizonMin: number; threshold?: number } {
  const q = obj(w.query);
  const h = typeof q.horizonMin === "number" && q.horizonMin > 0 ? q.horizonMin : 360;
  const t = typeof q.threshold === "number" && Number.isFinite(q.threshold) ? q.threshold : undefined;
  return { horizonMin: h, threshold: t };
}

export interface MetricQuery {
  queries: MetricDataQuery[];
  period?: number;
}

const obj = (q: unknown): Record<string, unknown> => (q && typeof q === "object" ? (q as Record<string, unknown>) : {});

export function metricQuery(w: Widget): MetricQuery {
  const q = obj(w.query);
  const queries = Array.isArray(q.queries) ? (q.queries as MetricDataQuery[]) : [];
  const period = typeof q.period === "number" && q.period > 0 ? q.period : undefined;
  return { queries, period };
}

export function logQuery(w: Widget): LogQuery {
  const q = obj(w.query);
  const s = (k: string) => (typeof q[k] === "string" && q[k] ? (q[k] as string) : undefined);
  return { logGroup: s("logGroup"), level: s("level"), query: s("query") };
}

export function textOf(w: Widget): string {
  const t = obj(w.query).text;
  return typeof t === "string" ? t : "";
}

export function alarmService(w: Widget): string | undefined {
  const s = obj(w.query).service;
  return typeof s === "string" && s ? s : undefined;
}

export function boardRange(d: Dashboard): string {
  return range(d.layout?.range).id;
}

export function boardRefresh(d: Dashboard): number {
  const r = d.layout?.refresh;
  return Math.max(MIN_REFRESH, typeof r === "number" && r > 0 ? r : DEFAULT_REFRESH);
}

/** The board's window, at the widget's own period when that is coarser than the range's. */
export function widgetWindow(rangeId: string, period?: number, now = Date.now()) {
  const base = timeWindow(rangeId, now);
  if (!period || period <= base.period) return base;
  const p = period * 1000;
  const end = Math.floor(now / p) * p;
  return { start: end - range(rangeId).ms, end, period };
}

/** Holt-Winters points per season: a day-shaped hour for minute data, else about an hour. */
export function seasonFor(period: number): number {
  return period === 60 ? 60 : Math.max(2, Math.round(3600 / period));
}

/** "service=<X>" when every metric query targets one named Service; else no filter. */
export function serviceFilter(queries: MetricDataQuery[]): string {
  const metric = queries.filter((q) => q.metricStat);
  if (!metric.length) return "";
  const svcs = new Set(metric.map((q) => q.metricStat?.dimensions?.Service));
  if (svcs.size !== 1) return "";
  const [s] = svcs;
  return s && s !== "*" ? `service=${s}` : "";
}

/** A unit when every metric query reads latency, which the platform stores in ms. */
export function unitFor(queries: MetricDataQuery[]): string | undefined {
  const metric = queries.filter((q) => q.metricStat);
  if (!metric.length) return undefined;
  if (queries.every((q) => q.metricStat?.metricName === "Latency")) return "ms";
  // ZeroWatch/Host percentages (CPUUtilization, MemoryUsedPercent, DiskUsedPercent, ...)
  if (queries.every((q) => /Percent$|^CPU(Utilization|IOWait|Steal)$/.test(q.metricStat?.metricName ?? ""))) return "%";
  return undefined;
}

export function nextQueryId(queries: MetricDataQuery[], prefix = "m"): string {
  const used = new Set(queries.map((q) => q.id));
  let n = 1;
  while (used.has(`${prefix}${n}`)) n++;
  return `${prefix}${n}`;
}

/** The server's rules (title, grid bounds, widget count), plus query shape checks, in plain words. */
export function validateBoard(d: Dashboard): string[] {
  const errs: string[] = [];
  if (!d.title.trim()) errs.push("Give the board a title.");
  else if (d.title.length > 200) errs.push("The title is longer than 200 characters.");
  if (d.widgets.length > MAX_WIDGETS) errs.push(`A board holds at most ${MAX_WIDGETS} widgets; this one has ${d.widgets.length}.`);
  d.widgets.forEach((w, i) => {
    const name = `Widget ${i + 1}${w.title ? ` (${w.title})` : ""}`;
    if (!(w.w > 0 && w.h > 0)) errs.push(`${name}: width and height must be at least 1.`);
    if (w.x < 0 || w.y < 0) errs.push(`${name}: position cannot be negative.`);
    if (w.x + w.w > GRID_COLUMNS) errs.push(`${name}: it runs past column ${GRID_COLUMNS} (x + width must be ${GRID_COLUMNS} or less).`);
    if (isMetricType(w.type)) {
      const { queries } = metricQuery(w);
      if (!queries.length) errs.push(`${name}: add at least one query.`);
      const ids = new Set<string>();
      queries.forEach((q, k) => {
        const qn = `${name}, query ${k + 1}`;
        if (!/^[a-z][A-Za-z0-9_]*$/.test(q.id)) errs.push(`${qn}: the id must start with a lower-case letter and use only letters, digits and _.`);
        else if (ids.has(q.id)) errs.push(`${qn}: the id "${q.id}" is used twice.`);
        ids.add(q.id);
        if (q.expression != null && !q.metricStat) {
          if (!q.expression.trim()) errs.push(`${qn}: the expression is empty.`);
        } else if (!q.metricStat?.namespace || !q.metricStat.metricName) errs.push(`${qn}: choose a metric.`);
      });
      if (queries.length && queries.every((q) => q.returnData === false)) errs.push(`${name}: every query is hidden, so nothing would be drawn.`);
    }
  });
  return errs;
}

/** Pairs of widgets that cover the same grid cells (allowed, but rarely meant). */
export function overlaps(widgets: Widget[]): [number, number][] {
  const out: [number, number][] = [];
  for (let i = 0; i < widgets.length; i++)
    for (let j = i + 1; j < widgets.length; j++) {
      const a = widgets[i];
      const b = widgets[j];
      if (a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h) out.push([i, j]);
    }
  return out;
}

/** Reading order on phones, where every widget is full width: by row, then column. */
export function readingOrder(widgets: Widget[]): number[] {
  return widgets.map((_, i) => i).sort((a, b) => widgets[a].y - widgets[b].y || widgets[a].x - widgets[b].x);
}
