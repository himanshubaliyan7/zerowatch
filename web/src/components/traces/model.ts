/**
 * A trace as the waterfall draws it: spans in tree order (depth-first by
 * parent, siblings by start), each with its depth and how many spans hang
 * under it, plus the trace's bounds and the self time spent in each service.
 * Spans whose parent never arrived are attached at root level and flagged.
 */
import type { Span } from "@/lib/api";
import { num } from "@/lib/format";

export interface SpanRow {
  span: Span;
  /** start..end in ms, with negative durations clamped to zero */
  end: number;
  depth: number;
  /** the span names a parent that is not in the trace */
  orphan: boolean;
  /** direct children */
  children: number;
  /** all spans below this one; the rows right after it in `rows` */
  descendants: number;
  /** own time: duration minus the time covered by its children */
  self: number;
}

export interface TraceModel {
  rows: SpanRow[];
  root: Span;
  t0: number;
  t1: number;
  total: number;
  services: string[];
  errors: number;
  orphans: number;
  /** self time per service, largest first */
  selfByService: { service: string; ms: number; spans: number }[];
}

const dur = (s: Span) => Math.max(0, Number.isFinite(s.durationMs) ? s.durationMs : 0);

/** Total length of the union of intervals, clipped to [lo, hi]. */
function covered(intervals: [number, number][], lo: number, hi: number): number {
  const iv = intervals
    .map(([a, b]): [number, number] => [Math.max(a, lo), Math.min(b, hi)])
    .filter(([a, b]) => b > a)
    .sort((a, b) => a[0] - b[0]);
  let sum = 0;
  let cur: [number, number] | null = null;
  for (const x of iv) {
    if (!cur || x[0] > cur[1]) {
      if (cur) sum += cur[1] - cur[0];
      cur = [x[0], x[1]];
    } else cur[1] = Math.max(cur[1], x[1]);
  }
  if (cur) sum += cur[1] - cur[0];
  return sum;
}

export function buildTrace(spans: Span[]): TraceModel | null {
  if (spans.length === 0) return null;
  const byId = new Map(spans.map((s) => [s.spanId, s]));
  const kids = new Map<string, Span[]>();
  const roots: Span[] = [];
  for (const s of spans) {
    const p = s.parentSpanId;
    if (p && p !== s.spanId && byId.has(p)) {
      let k = kids.get(p);
      if (!k) kids.set(p, (k = []));
      k.push(s);
    } else roots.push(s);
  }
  const byStart = (a: Span, b: Span) => a.start - b.start || a.name.localeCompare(b.name);
  roots.sort((a, b) => Number(!!a.parentSpanId) - Number(!!b.parentSpanId) || byStart(a, b));
  for (const k of kids.values()) k.sort(byStart);

  const rows: SpanRow[] = [];
  const seen = new Set<string>();
  const visit = (s: Span, depth: number) => {
    if (seen.has(s.spanId)) return;
    seen.add(s.spanId);
    const end = s.start + dur(s);
    const children = kids.get(s.spanId) ?? [];
    const row: SpanRow = {
      span: s,
      end,
      depth,
      orphan: !!s.parentSpanId && !byId.has(s.parentSpanId),
      children: children.length,
      descendants: 0,
      self: Math.max(0, dur(s) - covered(children.map((c) => [c.start, c.start + dur(c)]), s.start, end)),
    };
    rows.push(row);
    const at = rows.length;
    for (const c of children) visit(c, depth + 1);
    row.descendants = rows.length - at;
  };
  for (const r of roots) visit(r, 0);
  // A parent cycle leaves spans unvisited; show them at root level rather than drop them.
  for (const s of spans) if (!seen.has(s.spanId)) visit(s, 0);

  const t0 = Math.min(...rows.map((r) => r.span.start));
  const t1 = Math.max(...rows.map((r) => r.end));
  const self = new Map<string, { ms: number; spans: number }>();
  for (const r of rows) {
    const e = self.get(r.span.service) ?? { ms: 0, spans: 0 };
    e.ms += r.self;
    e.spans++;
    self.set(r.span.service, e);
  }
  return {
    rows,
    root: rows[0].span,
    t0,
    t1,
    total: Math.max(t1 - t0, 0.001),
    services: [...self.keys()].sort(),
    errors: rows.filter((r) => r.span.status === "error").length,
    orphans: rows.filter((r) => r.orphan).length,
    selfByService: [...self.entries()].map(([service, v]) => ({ service, ...v })).sort((a, b) => b.ms - a.ms || a.service.localeCompare(b.service)),
  };
}

/** The rows left showing when the spans in `collapsed` hide their subtrees. */
export function visibleRows(rows: SpanRow[], collapsed: ReadonlySet<string>): SpanRow[] {
  const out: SpanRow[] = [];
  for (let i = 0; i < rows.length; i++) {
    out.push(rows[i]);
    if (collapsed.has(rows[i].span.spanId)) i += rows[i].descendants;
  }
  return out;
}

/** Durations at the precision a span needs: 0.42 ms, 6.4 ms, 212 ms, 12.5 s. */
export function fmtMs(v: number): string {
  if (!Number.isFinite(v)) return "—";
  const a = Math.abs(v);
  if (a >= 10_000) return `${num(v / 1000, 1)} s`;
  if (a >= 100 || a === 0) return `${num(v)} ms`;
  if (a >= 10) return `${num(v, 1)} ms`;
  return `${num(v, 2)} ms`;
}

/** Evenly spaced round ticks (1, 2 or 5 × 10ⁿ) from 0 to `total`. */
export function niceTicks(total: number, target = 5): number[] {
  const raw = total / target;
  const pow = 10 ** Math.floor(Math.log10(raw));
  const m = raw / pow;
  const step = (m < 1.5 ? 1 : m < 3.5 ? 2 : m < 7.5 ? 5 : 10) * pow;
  const out: number[] = [];
  for (let v = 0; v <= total + step * 1e-9; v += step) out.push(v);
  return out;
}

/** The trace id at a readable length. */
export const shortId = (id: string) => (id.length > 12 ? `${id.slice(0, 12)}…` : id);
