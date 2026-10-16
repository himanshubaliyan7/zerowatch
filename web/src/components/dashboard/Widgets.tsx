/**
 * One renderer per widget type. Each widget fetches its own data on the
 * board's refresh interval, so a slow or failing query blanks only itself.
 */
import type { CSSProperties, ReactNode } from "react";
import { endpoints, type AlarmRule, type ForecastRequest, type ForecastResult, type QueryResult, type Widget } from "@/lib/api";
import { usePoll } from "@/lib/hooks";
import { ALARM_STATE, describeLog, levelTone } from "@/lib/status";
import { clock, plural } from "@/lib/format";
import { logsLink } from "@/lib/range";
import { Empty, ErrorNote, Skeleton, Stat, StatusMark, Verdict } from "@/design/primitives";
import { toneText, typography } from "@/design/tokens";
import { MultiPlot, PlotLegend, type PlotBand, type PlotSeries } from "@/components/MultiPlot";
import { alarmService, anomalyMethod, forecastOf, logQuery, metricQuery, seasonFor, serviceFilter, textOf, unitFor, widgetWindow } from "./model";

export interface WidgetProps {
  widget: Widget;
  rangeId: string;
  refreshMs: number;
}

export function WidgetBody(props: WidgetProps) {
  switch (props.widget.type) {
    case "stat":
      return <StatWidget {...props} />;
    case "timeseries":
      return <TimeseriesWidget {...props} />;
    case "anomaly":
      return <AnomalyWidget {...props} />;
    case "forecast":
      return <ForecastWidget {...props} />;
    case "logs":
      return <LogsWidget {...props} />;
    case "alarm":
      return <AlarmWidget {...props} />;
    case "text":
      return <TextWidget widget={props.widget} />;
    default:
      return <Empty>This widget type is not known to this version of the UI.</Empty>;
  }
}

/** Plot height in viewBox units, from the widget's height in grid rows. */
const plotHeight = (h: number) => Math.min(420, Math.max(150, h * 64));

const NoData = ({ className = "" }: { className?: string }) => (
  <div className={`hatch flex items-center justify-center border border-border p-6 text-center text-[0.95rem] text-muted-foreground ${className}`}>
    No data in this window.
  </div>
);

const Loading = ({ className = "h-40" }: { className?: string }) => <Skeleton className={`w-full ${className}`} />;

// ---- metric widgets ----

interface MetricData {
  results: QueryResult[];
  period: number;
}

function useMetricData({ widget, rangeId, refreshMs }: WidgetProps) {
  const mq = metricQuery(widget);
  const key = `${rangeId}|${JSON.stringify(mq)}`;
  return usePoll<MetricData>(
    async (signal) => {
      const win = widgetWindow(rangeId, mq.period);
      if (!mq.queries.length) return { results: [], period: win.period };
      const res = await endpoints.query({ queries: mq.queries, start: win.start, end: win.end, period: win.period }, signal);
      const hidden = new Set(mq.queries.filter((q) => q.returnData === false).map((q) => q.id));
      return { results: res.filter((r) => !hidden.has(r.id)), period: win.period };
    },
    refreshMs,
    [key],
  );
}

const usable = (r: QueryResult) => r.statusCode !== "InternalError";

/** Messages the query engine returned (bad expression, partial data), said as written. */
function QueryNotes({ results }: { results: QueryResult[] }) {
  const notes = results.filter((r) => r.statusCode !== "Complete" || r.messages?.length);
  if (!notes.length) return null;
  return (
    <ul className="mt-2.5 space-y-1 text-[0.85rem] leading-snug">
      {notes.map((r) => (
        <li key={r.id}>
          <span className={`${typography.micro} mr-2`}>{r.label || r.id}</span>
          {r.statusCode === "PartialData" && "Partial data: some storage shards did not answer, so values may be low. "}
          {r.messages?.length ? r.messages.join("; ") : r.statusCode === "InternalError" ? "The query failed." : ""}
        </li>
      ))}
    </ul>
  );
}

function StatWidget(props: WidgetProps) {
  const data = useMetricData(props);
  if (data.error && !data.data) return <ErrorNote error={data.error} what="this stat" />;
  if (!data.data) return <Loading className="h-28" />;
  const first = data.data.results.find(usable);
  const value = first && first.values.length ? first.values[first.values.length - 1] : null;
  const abs = Math.abs(value ?? 0);
  return (
    <div>
      {value == null || !Number.isFinite(value) ? (
        <NoData className="min-h-28" />
      ) : (
        <div className="border-l border-t border-border">
          <Stat
            label={first?.label || first?.id || "value"}
            value={value}
            decimals={abs > 0 && abs < 10 ? 2 : abs < 100 ? 1 : 0}
            unit={`latest ${periodWords(data.data.period)}${unitFor(metricQuery(props.widget).queries) ? ` · ${unitFor(metricQuery(props.widget).queries)}` : ""}`}
          />
        </div>
      )}
      <QueryNotes results={data.data.results} />
    </div>
  );
}

function periodWords(s: number): string {
  return s % 3600 === 0 ? `${s / 3600} h` : s % 60 === 0 ? `${s / 60} min` : `${s} s`;
}

const toSeries = (r: QueryResult): PlotSeries => ({ label: r.label || r.id, t: r.timestamps, v: r.values });

function openLogs(t: number, q: string) {
  window.location.hash = logsLink(t - 30_000, t + 30_000, q);
}

function TimeseriesWidget(props: WidgetProps) {
  const data = useMetricData(props);
  const { queries } = metricQuery(props.widget);
  if (data.error && !data.data) return <ErrorNote error={data.error} what="this chart" />;
  if (!data.data) return <Loading />;
  const series = data.data.results.filter(usable).map(toSeries);
  const filter = serviceFilter(queries);
  return (
    <div>
      <MultiPlot
        series={series}
        height={plotHeight(props.widget.h)}
        unit={unitFor(queries)}
        onPick={(t) => openLogs(t, filter)}
        pickLabel={filter ? `Logs for ${filter.slice(8)}, ±30 s` : "Logs for ±30 s"}
        ariaLabel={`${props.widget.title || "Time series"}: ${series.map((s) => s.label).join(", ")}`}
      />
      {series.length > 0 && <PlotLegend labels={series.map((s) => s.label)} />}
      <QueryNotes results={data.data.results} />
    </div>
  );
}

interface AnomalyData extends MetricData {
  band?: PlotBand;
}

function AnomalyWidget(props: WidgetProps) {
  const mq = metricQuery(props.widget);
  const key = `${props.rangeId}|${JSON.stringify(mq)}`;
  const data = usePoll<AnomalyData>(
    async (signal) => {
      const win = widgetWindow(props.rangeId, mq.period);
      if (!mq.queries.length) return { results: [], period: win.period };
      const res = await endpoints.query({ queries: mq.queries, start: win.start, end: win.end, period: win.period }, signal);
      const hidden = new Set(mq.queries.filter((q) => q.returnData === false).map((q) => q.id));
      const results = res.filter((r) => !hidden.has(r.id));
      const first = results.find(usable);
      if (!first || first.values.length < 2) return { results, period: win.period };
      const bands = await endpoints.anomaly(first.values, seasonFor(win.period), signal, anomalyMethod(props.widget));
      const band: PlotBand = {
        t: first.timestamps,
        lower: first.timestamps.map((_, i) => bands[i]?.lower ?? null),
        upper: first.timestamps.map((_, i) => bands[i]?.upper ?? null),
        anomalous: bands.flatMap((b, i) => (b.anomalous && i < first.timestamps.length ? [i] : [])),
      };
      return { results, period: win.period, band };
    },
    props.refreshMs,
    [key],
  );
  if (data.error && !data.data) return <ErrorNote error={data.error} what="this anomaly band" />;
  if (!data.data) return <Loading />;
  const first = data.data.results.find(usable);
  const series = first ? [toSeries(first)] : [];
  const band = data.data.band;
  const flagged = band?.anomalous?.length ?? 0;
  const filter = serviceFilter(mq.queries);
  const warming = band ? band.lower.every((v) => v == null) : false;
  return (
    <div>
      <MultiPlot
        series={series}
        band={band}
        height={plotHeight(props.widget.h)}
        unit={unitFor(mq.queries)}
        onPick={(t) => openLogs(t, filter)}
        pickLabel={filter ? `Logs for ${filter.slice(8)}, ±30 s` : "Logs for ±30 s"}
        ariaLabel={`${props.widget.title || "Anomaly"}: ${series[0]?.label ?? "no series"} with its expected range, ${plural(flagged, "anomalous point")}`}
      />
      {series.length > 0 && (
        <div className="flex flex-wrap items-end justify-between gap-x-5">
          <PlotLegend labels={series.map((s) => s.label)} band={`expected range (${anomalyMethod(props.widget) === "stl" ? "STL" : "Holt-Winters"})`} />
          <span className="mt-2.5">
            {warming ? (
              <StatusMark tone="unknown" label="model warming up: one season of data first" />
            ) : flagged ? (
              <StatusMark tone="critical" label={`${plural(flagged, "anomalous point")}`} />
            ) : (
              <span className={typography.eyebrow}>no point outside the range</span>
            )}
          </span>
        </div>
      )}
      <QueryNotes results={data.data.results} />
    </div>
  );
}

interface ForecastData extends MetricData {
  forecast?: ForecastResult;
  forecastError?: string;
}

/**
 * The first shown series over the board's range, continued by its forecast.
 * A metric query is forecast from its stored history (the server picks the
 * span); an expression from the plotted values.
 */
function ForecastWidget(props: WidgetProps) {
  const mq = metricQuery(props.widget);
  const { horizonMin, threshold } = forecastOf(props.widget);
  const key = `${props.rangeId}|${JSON.stringify(props.widget.query)}`;
  const data = usePoll<ForecastData>(
    async (signal) => {
      const win = widgetWindow(props.rangeId, mq.period);
      if (!mq.queries.length) return { results: [], period: win.period };
      const res = await endpoints.query({ queries: mq.queries, start: win.start, end: win.end, period: win.period }, signal);
      const hidden = new Set(mq.queries.filter((q) => q.returnData === false).map((q) => q.id));
      const results = res.filter((r) => !hidden.has(r.id));
      const first = results.find(usable);
      if (!first) return { results, period: win.period };
      const q = mq.queries.find((x) => x.id === first.id);
      const req: ForecastRequest = { period: win.period, horizon: Math.max(1, Math.round((horizonMin * 60) / win.period)), end: win.end };
      if (q?.metricStat) req.metricStat = q.metricStat;
      else {
        const byT = new Map(first.timestamps.map((t, i) => [t, first.values[i]]));
        const p = win.period * 1000;
        const n = Math.round((win.end - win.start) / p);
        req.values = Array.from({ length: n }, (_, i) => byT.get(win.start + i * p) ?? null);
      }
      if (threshold != null) Object.assign(req, { threshold, comparison: "GreaterThanThreshold" });
      try {
        return { results, period: win.period, forecast: await endpoints.forecast(req, signal) };
      } catch (e) {
        if (e instanceof DOMException && e.name === "AbortError") throw e;
        return { results, period: win.period, forecastError: e instanceof Error ? e.message : String(e) };
      }
    },
    Math.max(props.refreshMs, 60_000), // a forecast moves slowly; at most one a minute
    [key],
  );
  if (data.error && !data.data) return <ErrorNote error={data.error} what="this forecast" />;
  if (!data.data) return <Loading />;
  const first = data.data.results.find(usable);
  const series = first ? [toSeries(first)] : [];
  const f = data.data.forecast;
  const span = horizonMin < 60 ? `${horizonMin} min` : `${horizonMin / 60} h`;
  return (
    <div>
      <MultiPlot
        series={series}
        threshold={threshold}
        forecast={
          f ? { t: f.points.map((p) => p.t), forecast: f.points.map((p) => p.forecast), lower: f.points.map((p) => p.lower), upper: f.points.map((p) => p.upper), crossesAt: f.crossesAt } : undefined
        }
        height={plotHeight(props.widget.h)}
        unit={unitFor(mq.queries)}
        ariaLabel={`${props.widget.title || "Forecast"}: ${series[0]?.label ?? "no series"} and its ${span} forecast`}
      />
      {series.length > 0 && (
        <div className="flex flex-wrap items-end justify-between gap-x-5">
          <PlotLegend labels={series.map((s) => s.label)} forecast={f ? "forecast" : undefined} band={f ? "80 % range" : undefined} threshold={threshold != null ? `threshold ${threshold}` : undefined} />
          <span className="mt-2.5">
            {data.data.forecastError ? (
              <StatusMark
                tone="unknown"
                label={/at least 3 values/.test(data.data.forecastError) ? "no forecast yet: needs 3 periods of history" : `no forecast: ${data.data.forecastError}`}
              />
            ) : f && threshold != null && f.crossesAt != null ? (
              <StatusMark tone="warning" label={`crosses ${threshold} at ${clock(f.crossesAt).slice(0, 5)}`} />
            ) : f && threshold != null ? (
              <span className={typography.eyebrow}>stays under {threshold} for {span}</span>
            ) : f ? (
              <span className={typography.eyebrow}>{f.method}</span>
            ) : null}
          </span>
        </div>
      )}
      <QueryNotes results={data.data.results} />
    </div>
  );
}

// ---- logs ----

function LogsWidget({ widget, rangeId, refreshMs }: WidgetProps) {
  const q = logQuery(widget);
  const key = `${rangeId}|${JSON.stringify(q)}`;
  const data = usePoll(
    (signal) => {
      const win = widgetWindow(rangeId);
      return endpoints.searchLogs({ ...q, start: win.start, end: Date.now(), limit: 50 }, signal);
    },
    refreshMs,
    [key],
  );
  if (data.error && !data.data) return <ErrorNote error={data.error} what="logs" />;
  if (!data.data) return <Loading className="h-32" />;
  const events = data.data.events;
  if (!events.length) return <NoData />;
  return (
    <div className="max-h-[28rem] overflow-y-auto border-b border-hair font-mono text-[0.75rem] leading-snug">
      {events.map((e, i) => {
        const tone = levelTone(e.level);
        return (
          <div
            key={`${e.timestamp}-${i}`}
            className="grid grid-cols-[4.25rem_3.25rem_minmax(0,1fr)] gap-x-3 border-t border-hair py-1.5 sm:grid-cols-[5.5rem_3.5rem_minmax(0,1fr)]"
          >
            <span className="tabular-nums text-muted-foreground">{clock(e.timestamp)}</span>
            <span className={`uppercase ${tone === "unknown" ? "text-muted-foreground" : toneText[tone]}`}>{e.level || "—"}</span>
            <span className="[overflow-wrap:anywhere]">{describeLog(e).text}</span>
          </div>
        );
      })}
      {data.data.truncated && <p className={`${typography.eyebrow} border-t border-hair py-1.5`}>latest 50 shown</p>}
    </div>
  );
}

// ---- alarms ----

const STATE = ALARM_STATE;

const rank = (r: AlarmRule) => (r.state === "ALARM" ? 0 : r.state === "OK" ? 2 : 1);

function AlarmWidget({ widget, refreshMs }: WidgetProps) {
  const service = alarmService(widget);
  const rules = usePoll(() => endpoints.alarms(), refreshMs);
  if (rules.error && !rules.data) return <ErrorNote error={rules.error} what="alarm rules" />;
  if (!rules.data) return <Loading className="h-24" />;
  const list = rules.data
    .filter((r) => !service || r.metric?.dimensions?.Service === service)
    .sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name));
  if (!list.length) return <Empty>{service ? `No alarm rules watch ${service}.` : "No alarm rules yet."}</Empty>;
  return (
    <div className="border-b border-hair">
      {list.map((r) => {
        const st = STATE[r.state] ?? STATE[""];
        return (
          <a
            key={r.id}
            href="#/alarms"
            className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 border-t border-hair py-2.5 transition-[background-color,padding] hover:bg-muted hover:pl-3"
          >
            <span className="min-w-0 text-[0.95rem] [overflow-wrap:anywhere]">
              {r.name}
              {!r.enabled && <span className={`${typography.eyebrow} ml-2`}>disabled</span>}
            </span>
            <Verdict tone={st.tone} label={st.label} />
          </a>
        );
      })}
    </div>
  );
}

// ---- text ----

function TextWidget({ widget }: { widget: Widget }) {
  const text = textOf(widget);
  if (!text.trim()) return <Empty>No text yet.</Empty>;
  return <p className={`${typography.body} max-w-[70ch] whitespace-pre-line [overflow-wrap:anywhere]`}>{text}</p>;
}

/** A widget on the board: micro title, its type, a rule, then the body. No card box. */
export function WidgetSection({ widget, index, children, style, className }: { widget: Widget; index: number; children: ReactNode; style?: CSSProperties; className?: string }) {
  return (
    <section className={`rise-in flex min-w-0 flex-col ${className ?? ""}`} style={{ ...style, "--i": index } as CSSProperties} aria-label={widget.title || "Untitled widget"}>
      <div className="mb-3 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 border-b border-border pb-2">
        <h2 className={`${typography.micro} min-w-0 [overflow-wrap:anywhere]`}>{widget.title || "Untitled widget"}</h2>
        <span className={typography.eyebrow}>{widget.type}</span>
      </div>
      <div className="min-w-0 flex-1">{children}</div>
    </section>
  );
}

