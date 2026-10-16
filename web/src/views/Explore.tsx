/**
 * The metric explorer: CloudWatch-style GetMetricData over stored series.
 * Metric rows (m1, m2 …) pick a stored series and statistic; expression rows
 * (e1, e2 …) do metric math over them. The query set lives in the URL hash
 * (#/explore?q=…), so a view can be shared; #/explore?m=<MetricStat> seeds m1.
 * Optional Holt-Winters band on the first returned series, a click through
 * to the logs around any moment, and "add to dashboard" / "create alarm".
 * A forecast continues the first returned series (POST /api/v1/forecast): a
 * metric row is forecast from its stored history, an expression from the
 * plotted values; with a threshold it says when the forecast crosses it.
 */
import { useEffect, useMemo, useRef, useState } from "react";
import {
  ApiError,
  endpoints,
  type AnomalyBand,
  type Dashboard,
  type ForecastRequest,
  type ForecastResult,
  type MetricDataQuery,
  type MetricStat,
  type QueryResult,
  type Widget,
} from "@/lib/api";
import { useDebounced, usePoll } from "@/lib/hooks";
import { clock, num, plural } from "@/lib/format";
import { RANGES, logsLink, range as rangeOf } from "@/lib/range";
import { Empty, ErrorNote, PageHead, Section, Skeleton, StatusMark } from "@/design/primitives";
import { control, layout, typography, type Tone } from "@/design/tokens";
import { MetricPicker, defaultMetric, useSeriesCatalog } from "@/components/MetricPicker";
import { MultiPlot, PlotLegend, type PlotBand, type PlotForecast, type PlotSeries } from "@/components/MultiPlot";

const ID_RE = /^[a-z][a-zA-Z0-9_]*$/;
/** Forecast horizons in minutes (0 = off). */
const HORIZONS = [
  { min: 0, label: "Off" },
  { min: 60, label: "1h" },
  { min: 360, label: "6h" },
  { min: 1440, label: "24h" },
] as const;
const METHODS = [
  { value: "auto", label: "auto" },
  { value: "holtwinters", label: "Holt-Winters" },
  { value: "stl", label: "STL" },
  { value: "holt", label: "damped Holt" },
  { value: "model", label: "model service" },
] as const;
type Method = (typeof METHODS)[number]["value"];
const BAND_METHODS = [
  { value: "holtwinters", label: "Holt-Winters (online)" },
  { value: "stl", label: "STL (robust, ≥ 2 seasons)" },
] as const;
type BandMethod = (typeof BAND_METHODS)[number]["value"];
const bandLabel = (m: BandMethod) => (m === "stl" ? "STL" : "Holt-Winters");
const PERIODS = [60, 300, 900, 3600] as const;
/** The values internal/query ParseFillPolicy accepts ("" = NONE). */
const FILLS = [
  { value: "", label: "none (gaps)" },
  { value: "0", label: "zero" },
  { value: "REPEAT", label: "repeat last" },
  { value: "LINEAR", label: "linear" },
] as const;
/** What internal/query/eval.go evaluates. */
const OPERATORS = "+  -  *  /  ^  ( )  unary -";
const FUNCTIONS = [
  ["AVG(m) SUM(m) MIN(m) MAX(m)", "one series: reduce over time to a number"],
  ["AVG(m1, m2, …) SUM MIN MAX", "several series: point by point across them"],
  ["RATE(m) DIFF(m)", "change per second / per point"],
  ["FILL(m, REPEAT | LINEAR | NONE | 0 | m2)", "fill gaps by a policy, a number or another series"],
  ["PERIOD(m)", "the period in seconds"],
  ["ABS CEIL FLOOR SQRT LOG LOG10", "point-wise maths"],
] as const;

type Row =
  | { key: number; kind: "metric"; id: string; label: string; show: boolean; stat: MetricStat }
  | { key: number; kind: "expr"; id: string; label: string; show: boolean; expression: string };

interface Shared {
  queries: MetricDataQuery[];
  range: string;
  period: number;
  fill: string;
  anomaly?: boolean;
  season?: number;
  /** anomaly band model */
  band?: BandMethod;
  /** forecast horizon in minutes */
  forecast?: number;
  method?: Method;
  threshold?: number;
}

let keySeq = 0;
const nextKey = () => ++keySeq;

function isMetricStat(v: unknown): v is MetricStat {
  if (!v || typeof v !== "object") return false;
  const m = v as Record<string, unknown>;
  const dimsOk =
    m.dimensions === undefined ||
    (typeof m.dimensions === "object" && m.dimensions !== null && Object.values(m.dimensions).every((x) => typeof x === "string"));
  return typeof m.namespace === "string" && typeof m.metricName === "string" && typeof m.stat === "string" && dimsOk;
}

function parseJson(s: string | null | undefined): unknown {
  if (!s) return undefined;
  try {
    return JSON.parse(s);
  } catch {
    return undefined;
  }
}

function toRows(queries: MetricDataQuery[]): Row[] {
  return queries.map((q) =>
    q.metricStat
      ? { key: nextKey(), kind: "metric", id: q.id, label: q.label ?? "", show: q.returnData !== false, stat: q.metricStat }
      : { key: nextKey(), kind: "expr", id: q.id, label: q.label ?? "", show: q.returnData !== false, expression: q.expression ?? "" },
  );
}

function toQueries(rows: Row[]): MetricDataQuery[] {
  return rows.map((r) => {
    const q: MetricDataQuery = { id: r.id };
    if (r.label.trim()) q.label = r.label.trim();
    if (r.kind === "metric") q.metricStat = r.stat;
    else q.expression = r.expression;
    if (!r.show) q.returnData = false;
    return q;
  });
}

/** The shared state in #/explore?q=…, if it parses. */
function readShared(): Shared | undefined {
  const qs = window.location.hash.split("?")[1] ?? "";
  const raw = parseJson(new URLSearchParams(qs).get("q"));
  if (!raw || typeof raw !== "object") return undefined;
  const s = raw as Partial<Shared>;
  if (!Array.isArray(s.queries)) return undefined;
  const queries = s.queries.filter(
    (q): q is MetricDataQuery =>
      !!q && typeof q === "object" && typeof q.id === "string" && (isMetricStat(q.metricStat) || typeof q.expression === "string"),
  );
  return {
    queries,
    range: typeof s.range === "string" ? rangeOf(s.range).id : "1h",
    period: PERIODS.includes(s.period as (typeof PERIODS)[number]) ? (s.period as number) : rangeOf(s.range).period,
    fill: FILLS.some((f) => f.value === s.fill) ? (s.fill as string) : "",
    anomaly: s.anomaly === true,
    season: typeof s.season === "number" && s.season >= 2 ? Math.round(s.season) : 60,
    band: s.band === "stl" ? "stl" : "holtwinters",
    forecast: HORIZONS.some((h) => h.min === s.forecast) ? s.forecast : 0,
    method: METHODS.some((m) => m.value === s.method) ? s.method : "auto",
    threshold: typeof s.threshold === "number" && Number.isFinite(s.threshold) ? s.threshold : undefined,
  };
}

function freeId(rows: Row[], prefix: "m" | "e"): string {
  const used = new Set(rows.map((r) => r.id));
  let n = 1;
  while (used.has(`${prefix}${n}`)) n++;
  return `${prefix}${n}`;
}

/** Per-row problems, keyed by row key. Empty map = the set can run. */
function validate(rows: Row[]): Map<number, string> {
  const errs = new Map<number, string>();
  const seen = new Map<string, number>();
  for (const r of rows) seen.set(r.id, (seen.get(r.id) ?? 0) + 1);
  for (const r of rows) {
    if (!ID_RE.test(r.id)) errs.set(r.key, "Id must start with a lowercase letter, then letters, digits or _.");
    else if ((seen.get(r.id) ?? 0) > 1) errs.set(r.key, `Id ${r.id} is used twice.`);
    else if (r.kind === "metric" && (!r.stat.namespace || !r.stat.metricName)) errs.set(r.key, "Pick a metric.");
    else if (r.kind === "expr" && !r.expression.trim()) errs.set(r.key, "Write an expression, e.g. m1 / m2 * 100.");
  }
  return errs;
}

function summary(values: number[]) {
  if (!values.length) return { min: null, avg: null, max: null, last: null };
  let min = Infinity;
  let max = -Infinity;
  let sum = 0;
  for (const v of values) {
    min = Math.min(min, v);
    max = Math.max(max, v);
    sum += v;
  }
  return { min, avg: sum / values.length, max, last: values[values.length - 1] };
}

function dp(v: number | null): number {
  if (v == null) return 0;
  const a = Math.abs(v);
  return a === 0 || a >= 100 ? 0 : a >= 10 ? 1 : 2;
}

const statusTone: Record<QueryResult["statusCode"], Tone> = { Complete: "positive", PartialData: "warning", InternalError: "critical" };

function Select({ label, value, options, onChange }: { label: string; value: string; options: readonly { value: string; label: string }[]; onChange: (v: string) => void }) {
  return (
    <label className="block min-w-0">
      <span className={`${typography.eyebrow} mb-1 block`}>{label}</span>
      <span className="relative block">
        <select className={control.select} value={value} onChange={(e) => onChange(e.target.value)}>
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[0.6rem]" aria-hidden="true">
          ▼
        </span>
      </span>
    </label>
  );
}

function Toggle({ checked, onChange, children }: { checked: boolean; onChange: (v: boolean) => void; children: string }) {
  return (
    <label className={`${typography.micro} inline-flex cursor-pointer items-center gap-2 whitespace-nowrap`}>
      <input type="checkbox" className="h-3.5 w-3.5" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      {children}
    </label>
  );
}

export function Explore({ initial }: { initial?: string }) {
  const catalog = useSeriesCatalog();
  const boot = useMemo(() => {
    const shared = readShared();
    const fcBoot = { forecast: shared?.forecast ?? 0, method: shared?.method ?? ("auto" as Method), threshold: shared?.threshold, band: shared?.band ?? ("holtwinters" as BandMethod) };
    if (shared && shared.queries.length) return { ...shared, ...fcBoot, anomaly: shared.anomaly ?? false, season: shared.season ?? 60, rows: toRows(shared.queries) };
    const m = parseJson(initial);
    const rows: Row[] = isMetricStat(m) ? [{ key: nextKey(), kind: "metric", id: "m1", label: "", show: true, stat: m }] : [];
    return { rows, ...fcBoot, range: shared?.range ?? "1h", period: shared?.period ?? 60, fill: shared?.fill ?? "", anomaly: shared?.anomaly ?? false, season: shared?.season ?? 60 };
    // Read once on mount; later changes are ours (replaceState).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const [rows, setRows] = useState<Row[]>(boot.rows);
  const [rangeId, setRangeId] = useState<string>(boot.range);
  const [period, setPeriod] = useState<number>(boot.period);
  const [fill, setFill] = useState<string>(boot.fill);
  const [anomaly, setAnomaly] = useState<boolean>(boot.anomaly);
  const [season, setSeason] = useState<number>(boot.season);
  const [bandMethod, setBandMethod] = useState<BandMethod>(boot.band);
  const [customSeason, setCustomSeason] = useState(boot.season !== 60);
  const [seasonText, setSeasonText] = useState<string>(String(boot.season));
  const [horizon, setHorizon] = useState<number>(boot.forecast);
  const [method, setMethod] = useState<Method>(boot.method);
  const [thresholdText, setThresholdText] = useState<string>(boot.threshold != null ? String(boot.threshold) : "");
  const threshold = thresholdText.trim() !== "" && Number.isFinite(Number(thresholdText)) ? Number(thresholdText) : undefined;
  const [nonce, setNonce] = useState(0);

  // Nothing to start from: seed m1 with a sensible stored metric once the catalog arrives.
  useEffect(() => {
    if (rows.length || !catalog.data?.length) return;
    setRows([{ key: nextKey(), kind: "metric", id: "m1", label: "", show: true, stat: defaultMetric(catalog.data) }]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [catalog.data]);

  const errors = useMemo(() => validate(rows), [rows]);
  const queries = useMemo(() => toQueries(rows), [rows]);

  // Keep the shareable state in the hash without a history entry (and without App remounting us).
  useEffect(() => {
    if (!rows.length) return;
    const shared: Shared = {
      queries,
      range: rangeId,
      period,
      fill,
      ...(anomaly ? { anomaly, season, band: bandMethod } : {}),
      ...(horizon ? { forecast: horizon, method } : {}),
      ...(threshold != null ? { threshold } : {}),
    };
    const hash = `#/explore?q=${encodeURIComponent(JSON.stringify(shared))}`;
    if (window.location.hash !== hash) history.replaceState(history.state, "", hash);
  }, [queries, rows.length, rangeId, period, fill, anomaly, season, bandMethod, horizon, method, threshold]);

  const requestKey = rows.length && errors.size === 0 ? JSON.stringify({ queries, range: rangeId, period, fill }) : "";
  const debouncedKey = useDebounced(requestKey, 400);

  const [results, setResults] = useState<QueryResult[]>();
  const [queryError, setQueryError] = useState<Error>();
  const [running, setRunning] = useState(false);
  const [ranAt, setRanAt] = useState<number>();

  useEffect(() => {
    if (!debouncedKey) return;
    const req = JSON.parse(debouncedKey) as { queries: MetricDataQuery[]; range: string; period: number; fill: string };
    const p = req.period * 1000;
    const end = Math.floor(Date.now() / p) * p;
    const start = end - rangeOf(req.range).ms;
    const ctrl = new AbortController();
    setRunning(true);
    endpoints
      .query({ queries: req.queries, start, end, period: req.period, ...(req.fill ? { fill: req.fill } : {}) }, ctrl.signal)
      .then((r) => {
        setResults(r);
        setQueryError(undefined);
        setRanAt(Date.now());
      })
      .catch((e: unknown) => {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setQueryError(e instanceof Error ? e : new Error(String(e)));
      })
      .finally(() => {
        if (!ctrl.signal.aborted) setRunning(false);
      });
    return () => ctrl.abort();
  }, [debouncedKey, nonce]);

  // Holt-Winters band over the first returned series that has data.
  const first = results?.find((r) => r.statusCode !== "InternalError" && r.values.length > 0);
  const debouncedSeason = useDebounced(season, 400);
  const [bands, setBands] = useState<AnomalyBand[]>();
  const [bandError, setBandError] = useState<Error>();
  useEffect(() => {
    setBands(undefined);
    setBandError(undefined);
    if (!anomaly || !first) return;
    const ctrl = new AbortController();
    endpoints
      .anomaly(first.values, debouncedSeason, ctrl.signal, bandMethod)
      .then(setBands)
      .catch((e: unknown) => {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setBandError(e instanceof Error ? e : new Error(String(e)));
      });
    return () => ctrl.abort();
  }, [anomaly, first, debouncedSeason, bandMethod]);

  // Forecast of the first returned series: from stored history for a metric row, else the plotted values.
  const [fc, setFc] = useState<{ res?: ForecastResult; error?: Error }>();
  const fcThreshold = useDebounced(threshold, 400);
  useEffect(() => {
    setFc(undefined);
    if (!horizon || !first) return;
    const row = rows.find((r) => r.id === first.id);
    const p = period * 1000;
    const end = Math.floor(Date.now() / p) * p;
    const req: ForecastRequest = { period, horizon: Math.round((horizon * 60) / period), method, end };
    if (row?.kind === "metric") req.metricStat = row.stat;
    else {
      const byT = new Map(first.timestamps.map((t, i) => [t, first.values[i]]));
      const n = Math.round(rangeOf(rangeId).ms / p);
      req.values = Array.from({ length: n }, (_, i) => byT.get(end - (n - i) * p) ?? null);
    }
    if (fcThreshold != null) Object.assign(req, { threshold: fcThreshold, comparison: "GreaterThanThreshold" });
    const ctrl = new AbortController();
    endpoints
      .forecast(req, ctrl.signal)
      .then((res) => setFc({ res }))
      .catch((e: unknown) => {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setFc({ error: e instanceof Error ? e : new Error(String(e)) });
      });
    return () => ctrl.abort();
    // rows are read for the first series' kind; reruns follow `first` (a new query result)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [horizon, method, first, fcThreshold]);
  const forecast: PlotForecast | undefined = fc?.res
    ? {
        t: fc.res.points.map((x) => x.t),
        forecast: fc.res.points.map((x) => x.forecast),
        lower: fc.res.points.map((x) => x.lower),
        upper: fc.res.points.map((x) => x.upper),
        crossesAt: fc.res.crossesAt,
      }
    : undefined;

  const plotted = (results ?? []).filter((r) => r.statusCode !== "InternalError" || r.values.length > 0);
  const series: PlotSeries[] = plotted.map((r) => ({ label: r.label || r.id, t: r.timestamps, v: r.values }));
  const band: PlotBand | undefined =
    anomaly && first && bands && bands.length === first.timestamps.length
      ? {
          t: first.timestamps,
          lower: bands.map((b) => b.lower),
          upper: bands.map((b) => b.upper),
          anomalous: bands.flatMap((b, i) => (b.anomalous ? [i] : [])),
        }
      : undefined;
  // MultiPlot marks anomalies and continues the forecast on series[0]; keep the first result first.
  if ((band || forecast) && first && series[0]?.label !== (first.label || first.id)) {
    const i = plotted.indexOf(first);
    if (i > 0) series.unshift(...series.splice(i, 1));
  }
  const warming = !!bands && bands.length > 0 && bands.every((b) => b.lower == null);

  const m1 = rows.find((r): r is Extract<Row, { kind: "metric" }> => r.kind === "metric");
  const service = m1?.stat.dimensions?.Service;
  const logsQuery = service && service !== "*" ? `service=${service}` : "";

  const update = (key: number, patch: Partial<Row>) => setRows((rs) => rs.map((r) => (r.key === key ? ({ ...r, ...patch } as Row) : r)));
  const addMetric = () =>
    setRows((rs) => [
      ...rs,
      { key: nextKey(), kind: "metric", id: freeId(rs, "m"), label: "", show: true, stat: catalog.data?.length ? defaultMetric(catalog.data) : { namespace: "", metricName: "", stat: "Average", dimensions: {} } },
    ]);
  const addExpr = () =>
    setRows((rs) => {
      const ids = rs.filter((r) => r.kind === "metric").map((r) => r.id);
      const expression = ids.length >= 2 ? `${ids[0]} / ${ids[1]} * 100` : ids.length ? `${ids[0]} * 2` : "";
      return [...rs, { key: nextKey(), kind: "expr", id: freeId(rs, "e"), label: "", show: true, expression }];
    });

  const pickRange = (id: string) => {
    setRangeId(id);
    setPeriod(rangeOf(id).period);
  };

  return (
    <div className={layout.stack}>
      <PageHead
        eyebrow="Explore · metric math over stored series"
        lines={["Ask the", "metrics."]}
        lead="Pick stored series, combine them with arithmetic and functions, and see the result as it would be evaluated for a dashboard or an alarm."
      />

      <Section
        index={3}
        title="Queries"
        note={plural(rows.length, "query", "queries")}
        action={
          <span className="flex flex-wrap gap-2">
            <button type="button" className={control.button} onClick={addMetric}>
              + Metric
            </button>
            <button type="button" className={control.button} onClick={addExpr}>
              + Expression
            </button>
          </span>
        }
      >
        {catalog.error && !catalog.data ? (
          <ErrorNote error={catalog.error} what="the series catalog" />
        ) : !catalog.data && rows.length === 0 ? (
          <div className="space-y-2">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : rows.length === 0 ? (
          <Empty hatch>No metrics stored yet, so there is nothing to query. Series appear once anything writes metrics.</Empty>
        ) : (
          <ol className="border-b border-hair">
            {rows.map((r) => {
              const err = errors.get(r.key);
              return (
                <li key={r.key} className="border-t border-hair py-4 first:border-t-0 first:pt-1">
                  <div className="mb-3 flex flex-wrap items-end gap-x-4 gap-y-2">
                    <label className="block w-24 shrink-0">
                      <span className={`${typography.eyebrow} mb-1 block`}>{r.kind === "metric" ? "Metric id" : "Expr id"}</span>
                      <input
                        className={`${control.input} w-full ${err && !ID_RE.test(r.id) ? "border-tone-critical" : ""}`}
                        value={r.id}
                        spellCheck={false}
                        aria-invalid={!!err}
                        onChange={(e) => update(r.key, { id: e.target.value.trim() })}
                      />
                    </label>
                    <label className="block min-w-0 flex-1 basis-40">
                      <span className={`${typography.eyebrow} mb-1 block`}>Label</span>
                      <input className={`${control.input} w-full`} value={r.label} placeholder={r.id} onChange={(e) => update(r.key, { label: e.target.value })} />
                    </label>
                    <span className="flex items-center gap-4 pb-2">
                      <Toggle checked={r.show} onChange={(show) => update(r.key, { show })}>
                        Show
                      </Toggle>
                      <button
                        type="button"
                        className={`${control.link} !py-0`}
                        onClick={() => setRows((rs) => rs.filter((x) => x.key !== r.key))}
                        aria-label={`Remove ${r.id}`}
                      >
                        Remove
                      </button>
                    </span>
                  </div>
                  {r.kind === "metric" ? (
                    <MetricPicker value={r.stat} onChange={(stat) => update(r.key, { stat })} catalog={catalog.data ?? []} />
                  ) : (
                    <label className="block">
                      <span className={`${typography.eyebrow} mb-1 block`}>Expression</span>
                      <input
                        className={`${control.input} w-full`}
                        value={r.expression}
                        spellCheck={false}
                        placeholder="m1 / m2 * 100"
                        onChange={(e) => update(r.key, { expression: e.target.value })}
                      />
                    </label>
                  )}
                  {err && (
                    <p className={`${typography.micro} mt-2 inline-flex items-center gap-2`} role="alert">
                      <span className="inline-block h-2.5 w-2.5 shrink-0 bg-tone-critical" aria-hidden="true" />
                      {err}
                    </p>
                  )}
                </li>
              );
            })}
          </ol>
        )}
        <details className="mt-4">
          <summary className={`${typography.eyebrow} cursor-pointer hover:text-foreground`}>Expression functions</summary>
          <div className="mt-3 space-y-1.5 text-[0.9rem]">
            <p>
              <span className={typography.eyebrow}>Operators </span>
              <code className="font-mono text-[0.8rem]">{OPERATORS}</code>, numbers and query ids.
            </p>
            {FUNCTIONS.map(([sig, what]) => (
              <p key={sig} className="border-t border-hair pt-1.5 sm:grid sm:grid-cols-[minmax(0,22rem)_1fr] sm:gap-4">
                <code className="block font-mono text-[0.8rem] [overflow-wrap:anywhere]">{sig}</code>
                <span className="text-muted-foreground">{what}</span>
              </p>
            ))}
            <p className="pt-1 text-muted-foreground">Unticked “show” rows are computed but not returned, so they can feed an expression without being drawn.</p>
          </div>
        </details>
      </Section>

      <Section
        index={4}
        title="Window"
        note={ranAt ? `${running ? "running…" : `ran ${clock(ranAt)}`}` : running ? "running…" : undefined}
      >
        <div className="flex flex-wrap items-end gap-x-6 gap-y-4">
          <div>
            <span className={`${typography.eyebrow} mb-1 block`}>Range</span>
            <div className="flex flex-wrap gap-1.5">
              {RANGES.map((r) => (
                <button key={r.id} type="button" className={r.id === rangeId ? control.buttonActive : control.button} onClick={() => pickRange(r.id)} aria-pressed={r.id === rangeId}>
                  {r.label}
                </button>
              ))}
            </div>
          </div>
          <div className="w-32">
            <Select label="Period" value={String(period)} options={PERIODS.map((p) => ({ value: String(p), label: p < 3600 ? `${p / 60} min` : "1 h" }))} onChange={(v) => setPeriod(Number(v))} />
          </div>
          <div className="w-40">
            <Select label="Fill gaps" value={fill} options={FILLS} onChange={setFill} />
          </div>
          <button type="button" className={control.buttonActive} onClick={() => setNonce((n) => n + 1)} disabled={!requestKey}>
            Run
          </button>
        </div>
        <div className="mt-5 flex flex-wrap items-end gap-x-6 gap-y-3">
          <span className="pb-2">
            <Toggle checked={anomaly} onChange={setAnomaly}>
              Anomaly band on first series
            </Toggle>
          </span>
          {anomaly && (
            <>
              <div className="w-56">
                <Select label="Model" value={bandMethod} options={BAND_METHODS} onChange={(v) => setBandMethod(v as BandMethod)} />
              </div>
              <div className="w-48">
                <Select
                  label="Season"
                  value={customSeason ? "custom" : "60"}
                  options={[
                    { value: "60", label: "hourly at 1-min (60)" },
                    { value: "custom", label: "custom points" },
                  ]}
                  onChange={(v) => {
                    setCustomSeason(v === "custom");
                    if (v === "60") setSeason(60);
                  }}
                />
              </div>
              {customSeason && (
                <label className="block w-36">
                  <span className={`${typography.eyebrow} mb-1 block`}>Points per season</span>
                  <input
                    className={`${control.input} w-full tabular-nums`}
                    inputMode="numeric"
                    value={seasonText}
                    placeholder="≥ 2"
                    onChange={(e) => {
                      setSeasonText(e.target.value);
                      const n = Math.round(Number(e.target.value));
                      if (Number.isFinite(n) && n >= 2) setSeason(n);
                    }}
                  />
                </label>
              )}
            </>
          )}
        </div>
        <div className="mt-5 flex flex-wrap items-end gap-x-6 gap-y-3">
          <div>
            <span className={`${typography.eyebrow} mb-1 block`}>Forecast first series</span>
            <div className="flex flex-wrap gap-1.5">
              {HORIZONS.map((h) => (
                <button key={h.min} type="button" className={h.min === horizon ? control.buttonActive : control.button} onClick={() => setHorizon(h.min)} aria-pressed={h.min === horizon}>
                  {h.label}
                </button>
              ))}
            </div>
          </div>
          {horizon > 0 && (
            <>
              <div className="w-44">
                <Select label="Method" value={method} options={METHODS} onChange={(v) => setMethod(v as Method)} />
              </div>
              <label className="block w-36">
                <span className={`${typography.eyebrow} mb-1 block`}>Threshold</span>
                <input
                  className={`${control.input} w-full tabular-nums`}
                  inputMode="decimal"
                  value={thresholdText}
                  placeholder="optional"
                  onChange={(e) => setThresholdText(e.target.value)}
                />
              </label>
            </>
          )}
        </div>
      </Section>

      <Section
        index={5}
        title="Result"
        note={logsQuery || plotted.length ? `click the plot: logs ±30 s${logsQuery ? ` · ${logsQuery}` : ""}` : undefined}
      >
        {errors.size > 0 ? (
          <Empty>Fix the marked queries above to run.</Empty>
        ) : queryError ? (
          <Empty>
            The query was refused: <span className="font-mono text-[0.8rem]">{queryError.message}</span>
          </Empty>
        ) : !results ? (
          <Skeleton className="h-56 w-full" />
        ) : results.length === 0 ? (
          <Empty>No query is shown. Tick “show” on at least one row.</Empty>
        ) : (
          <div className="space-y-6">
            {results.some((r) => r.statusCode === "PartialData") && (
              <p className="text-[0.95rem]" role="status">
                <StatusMark tone="warning" label="Partial data" />{" "}
                <span className="ml-2">Some storage shards did not answer, so these values may be too low. The result table names the series.</span>
              </p>
            )}
            <div className="wipe">
              <MultiPlot
                series={series}
                band={band}
                forecast={forecast}
                threshold={horizon ? threshold : undefined}
                ariaLabel={`Query results: ${series.map((s) => s.label).join(", ")}${forecast ? `, with a ${horizon / 60} h forecast` : ""}`}
                onPick={(t) => {
                  window.location.hash = logsLink(t - 30_000, t + 30_000, logsQuery);
                }}
              />
              <PlotLegend
                labels={series.map((s) => s.label)}
                band={band ? `expected range (${bandLabel(bandMethod)})` : forecast ? "80 % forecast range" : undefined}
                forecast={forecast ? "forecast" : undefined}
                threshold={horizon && threshold != null ? `threshold ${num(threshold, dp(threshold))}` : undefined}
              />
            </div>

            {horizon > 0 && <ForecastNote fc={fc} first={first} horizon={horizon} threshold={threshold} />}

            <ResultTable results={results} />

            {anomaly && <AnomalyNote first={first} bands={bands} error={bandError} warming={warming} season={debouncedSeason} />}
          </div>
        )}
      </Section>

      <Section index={6} title="Use it">
        <Actions m1={m1?.stat} forecastSec={horizon * 60} bandMethod={bandMethod} queries={queries} period={period} anomaly={anomaly} canSave={errors.size === 0 && rows.length > 0} title={series.map((s) => s.label).join(" · ") || "Explore query"} />
      </Section>
    </div>
  );
}

function ResultTable({ results }: { results: QueryResult[] }) {
  return (
    <div className="relative overflow-x-auto">
      <table className="w-full min-w-[34rem] border-collapse text-left">
        <thead>
          <tr className={typography.eyebrow}>
            <th className="pb-2.5 pr-3 font-medium">Series</th>
            <th className="pb-2.5 pr-3 font-medium">Status</th>
            <th className="pb-2.5 pr-3 text-right font-medium">Points</th>
            <th className="pb-2.5 pr-3 text-right font-medium">Min</th>
            <th className="pb-2.5 pr-3 text-right font-medium">Avg</th>
            <th className="pb-2.5 pr-3 text-right font-medium">Max</th>
            <th className="pb-2.5 text-right font-medium">Last</th>
          </tr>
        </thead>
        <tbody>
          {results.map((r) => {
            const s = summary(r.values);
            return (
              <tr key={r.id} className="border-t border-hair align-top">
                <td className="py-2.5 pr-3">
                  <span className="font-mono text-[0.8rem]">{r.id}</span>
                  {r.label && r.label !== r.id && <span className="ml-2">{r.label}</span>}
                  {r.messages?.map((m, i) => (
                    <p key={i} className="mt-1 font-mono text-[0.75rem] text-muted-foreground [overflow-wrap:anywhere]">
                      {m}
                    </p>
                  ))}
                </td>
                <td className="py-2.5 pr-3">
                  <StatusMark tone={statusTone[r.statusCode] ?? "unknown"} label={r.statusCode} />
                </td>
                <td className={`py-2.5 pr-3 text-right ${typography.number}`}>{num(r.values.length)}</td>
                <td className={`py-2.5 pr-3 text-right ${typography.number}`}>{num(s.min, dp(s.min))}</td>
                <td className={`py-2.5 pr-3 text-right ${typography.number}`}>{num(s.avg, dp(s.avg))}</td>
                <td className={`py-2.5 pr-3 text-right ${typography.number}`}>{num(s.max, dp(s.max))}</td>
                <td className={`py-2.5 text-right ${typography.number}`}>{num(s.last, dp(s.last))}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function ForecastNote({ fc, first, horizon, threshold }: { fc?: { res?: ForecastResult; error?: Error }; first?: QueryResult; horizon: number; threshold?: number }) {
  if (!first) return <Empty hatch>No returned series has data to forecast.</Empty>;
  if (fc?.error) return <ErrorNote error={fc.error} what="the forecast" />;
  if (!fc?.res) return <Skeleton className="h-12 w-full" />;
  const r = fc.res;
  const inMin = r.crossesAt != null ? Math.max(0, Math.round((r.crossesAt - Date.now()) / 60_000)) : 0;
  const lead = threshold == null ? null : r.crossesAt != null ? (
    <p className={typography.lead}>
      <span className="mr-2 inline-block h-2.5 w-2.5 bg-tone-warning align-[0.1em]" aria-hidden="true" />
      <b>
        Forecast crosses {num(threshold, dp(threshold))} at {clock(r.crossesAt).slice(0, 5)}
      </b>
      , in {inMin < 120 ? plural(inMin, "minute") : `${num(inMin / 60, 1)} hours`}.
    </p>
  ) : (
    <p className={typography.lead}>
      The forecast stays below {num(threshold, dp(threshold))} for the next {horizon / 60} h.
    </p>
  );
  return (
    <div className="space-y-2">
      {lead}
      <p className={typography.eyebrow}>
        {first.label || first.id} · method {r.method}
        {r.fallback ? ` · ${r.fallback}` : ""}
      </p>
    </div>
  );
}

function AnomalyNote({ first, bands, error, warming, season }: { first?: QueryResult; bands?: AnomalyBand[]; error?: Error; warming: boolean; season: number }) {
  if (!first) return <Empty hatch>No returned series has data to model.</Empty>;
  if (error) return <ErrorNote error={error} what="the anomaly band" />;
  if (!bands) return <Skeleton className="h-16 w-full" />;
  if (warming)
    return (
      <Empty hatch>
        Model warming up for the first season: {plural(first.values.length, "point")} so far, the band appears after {plural(season, "point")}.
      </Empty>
    );
  const hits = bands.flatMap((b, i) => (b.anomalous ? [{ b, t: first.timestamps[i] }] : []));
  return (
    <div>
      <h3 className={`${typography.micro} mb-2`}>
        Anomalous points in {first.label || first.id} · {hits.length ? plural(hits.length, "point") : "none"}
      </h3>
      {hits.length === 0 ? (
        <p className="text-[0.95rem] text-muted-foreground">Every modelled point is inside the expected range.</p>
      ) : (
        <div className="relative max-h-72 overflow-auto">
          <table className="w-full min-w-[28rem] border-collapse text-left">
            <thead>
              <tr className={typography.eyebrow}>
                <th className="pb-2 pr-3 font-medium">Time</th>
                <th className="pb-2 pr-3 text-right font-medium">Value</th>
                <th className="pb-2 pr-3 text-right font-medium">Expected range</th>
                <th className="pb-2 font-medium">Status</th>
              </tr>
            </thead>
            <tbody>
              {hits.map(({ b, t }) => (
                <tr key={t} className="border-t border-hair">
                  <td className={`py-2 pr-3 ${typography.number}`}>{clock(t)}</td>
                  <td className={`py-2 pr-3 text-right ${typography.number}`}>{num(b.actual, dp(b.actual))}</td>
                  <td className={`py-2 pr-3 text-right ${typography.number}`}>
                    {num(b.lower, dp(b.lower))} – {num(b.upper, dp(b.upper))}
                  </td>
                  <td className="py-2">
                    <StatusMark tone="critical" label="anomalous" />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function Actions({
  m1,
  forecastSec,
  bandMethod,
  queries,
  period,
  anomaly,
  canSave,
  title: suggested,
}: {
  m1?: MetricStat;
  /** the forecast horizon shown (0 = none): offers a predictive alarm */
  forecastSec: number;
  bandMethod: BandMethod;
  queries: MetricDataQuery[];
  period: number;
  anomaly: boolean;
  canSave: boolean;
  title: string;
}) {
  const boards = usePoll(() => endpoints.dashboards(), 60_000);
  const [board, setBoard] = useState("");
  const [title, setTitle] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState<Dashboard>();
  const [saveError, setSaveError] = useState<Error>();
  const alive = useRef(true);
  useEffect(() => () => void (alive.current = false), []);

  const boardId = board || boards.data?.[0]?.id || "";

  const add = async () => {
    if (!boardId) return;
    setSaving(true);
    setSaved(undefined);
    setSaveError(undefined);
    const widgetTitle = title.trim() || suggested;
    try {
      // Compare-and-swap: on 409 someone saved first; refetch once and retry.
      for (let attempt = 0; ; attempt++) {
        const d = await endpoints.dashboard(boardId);
        const y = d.widgets.reduce((m, w) => Math.max(m, w.y + w.h), 0);
        const widget: Widget = { type: anomaly ? "anomaly" : "timeseries", title: widgetTitle, query: anomaly ? { queries, period, method: bandMethod } : { queries, period }, x: 0, y, w: 12, h: 4 };
        try {
          const out = await endpoints.saveDashboard({ ...d, widgets: [...d.widgets, widget] });
          if (alive.current) setSaved(out);
          break;
        } catch (e) {
          if (e instanceof ApiError && e.status === 409 && attempt === 0) continue;
          throw e;
        }
      }
    } catch (e) {
      if (alive.current) setSaveError(e instanceof Error ? e : new Error(String(e)));
    } finally {
      if (alive.current) setSaving(false);
    }
  };

  return (
    <div className="grid gap-8 lg:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">
      <div>
        <h3 className={`${typography.eyebrow} mb-3`}>Alarm</h3>
        {m1 && m1.namespace ? (
          <span className="flex flex-wrap gap-2">
            <a className={control.button} href={`#/alarms?new=${encodeURIComponent(JSON.stringify(m1))}`}>
              Create alarm from {m1.metricName} →
            </a>
            {forecastSec > 0 && (
              <a className={control.button} href={`#/alarms?new=${encodeURIComponent(JSON.stringify(m1))}&forecast=${forecastSec}`}>
                Alarm on the {forecastSec / 3600} h forecast →
              </a>
            )}
          </span>
        ) : (
          <p className="text-[0.95rem] text-muted-foreground">Add a metric row to alarm on it.</p>
        )}
      </div>
      <div className="min-w-0">
        <h3 className={`${typography.eyebrow} mb-3`}>Add to dashboard · as {anomaly ? "anomaly" : "time-series"} widget</h3>
        {boards.error && !boards.data ? (
          <ErrorNote error={boards.error} what="dashboards" />
        ) : !boards.data ? (
          <Skeleton className="h-10 w-full" />
        ) : boards.data.length === 0 ? (
          <Empty>
            No dashboards yet. <a className={control.link} href="#/dashboards">Make one</a> first.
          </Empty>
        ) : (
          <div className="flex flex-wrap items-end gap-3">
            <div className="w-full min-w-0 sm:w-56">
              <Select label="Dashboard" value={boardId} options={boards.data.map((b) => ({ value: b.id, label: b.title }))} onChange={setBoard} />
            </div>
            <label className="block min-w-0 flex-1 basis-48">
              <span className={`${typography.eyebrow} mb-1 block`}>Widget title</span>
              <input className={`${control.input} w-full`} value={title} placeholder={suggested} onChange={(e) => setTitle(e.target.value)} />
            </label>
            <button type="button" className={control.button} onClick={() => void add()} disabled={!canSave || saving}>
              {saving ? "Adding…" : "Add widget"}
            </button>
          </div>
        )}
        {saved && (
          <p className="mt-3 text-[0.95rem]">
            Added to {saved.title}.{" "}
            <a className={control.link} href={`#/dashboards?id=${encodeURIComponent(saved.id ?? boardId)}`}>
              Open the dashboard →
            </a>
          </p>
        )}
        {saveError && (
          <p className="mt-3 text-[0.95rem]">
            Could not add the widget: <span className="font-mono text-[0.8rem]">{saveError.message}</span>
          </p>
        )}
      </div>
    </div>
  );
}
