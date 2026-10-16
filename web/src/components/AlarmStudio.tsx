/**
 * Alarm Studio: create or edit one alarm rule, with a threshold simulator.
 * The rule's metric (or expression over m1) is fetched for the last 24 h at
 * the chosen period, plotted with the threshold, and replayed through the
 * server's M-of-N state machine (POST /api/v1/alarms/simulate) on every edit,
 * so the author sees how often the rule would have fired before saving it.
 *
 * A rule can instead evaluate a forecast (forecastHorizonSec): each period
 * the server forecasts the next N seconds and steps the state machine with
 * the worst forecast value. The simulator still replays the observed values,
 * and draws the current forecast past NOW with where it crosses.
 *
 * Validation mirrors alarms.Rule.Validate and alarms.Config.Validate; the
 * server still checks everything (the expression parser in particular).
 */
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { endpoints, type AlarmConfig, type AlarmRule, type AlarmState, type BacktestResult, type ForecastResult, type MetricStat, type SimulationResult } from "@/lib/api";
import { useDebounced } from "@/lib/hooks";
import { clock, comparison as compareSign, num, plural } from "@/lib/format";
import { defaultMetric, MetricPicker, useSeriesCatalog } from "@/components/MetricPicker";
import { MultiPlot, PLOT_PAD, PlotLegend } from "@/components/MultiPlot";
import { Skeleton, Stat, StatGrid, stagger } from "@/design/primitives";
import { control, typography } from "@/design/tokens";

const DAY = 24 * 60 * 60_000;

const PERIODS = [
  { sec: 60, label: "1 min" },
  { sec: 300, label: "5 min" },
  { sec: 900, label: "15 min" },
  { sec: 3600, label: "1 h" },
] as const;

/** Predictive horizons; only those of at least one period are offered (alarms.Rule.Validate). */
const HORIZONS = [
  { sec: 900, label: "15 min" },
  { sec: 3600, label: "1 hour" },
  { sec: 6 * 3600, label: "6 hours" },
  { sec: 86400, label: "24 hours" },
] as const;

const COMPARISONS = [
  { id: "GreaterThanThreshold", words: "greater than" },
  { id: "GreaterThanOrEqualToThreshold", words: "greater than or equal to" },
  { id: "LessThanThreshold", words: "less than" },
  { id: "LessThanOrEqualToThreshold", words: "less than or equal to" },
] as const;

const MISSING = [
  { id: "missing", words: "Missing", explain: "A gap counts as neither; with no data in the window the rule reports insufficient data." },
  { id: "breaching", words: "Breaching", explain: "A gap counts as a bad datapoint: silence itself raises the alarm." },
  { id: "notBreaching", words: "Not breaching", explain: "A gap counts as a good datapoint: silence keeps the rule OK." },
  { id: "ignore", words: "Ignore", explain: "A gap is skipped: the rule keeps its state and the window does not move." },
] as const;

/** notify.go Target: webhook and slack need url, pagerduty needs routingKey, log needs nothing. */
export interface Target {
  type: string;
  url?: string;
  routingKey?: string;
}

const ACTION_TYPES = [
  { id: "webhook", label: "Webhook" },
  { id: "slack", label: "Slack" },
  { id: "pagerduty", label: "PagerDuty" },
  { id: "sns", label: "Amazon SNS" },
  { id: "log", label: "Server log" },
] as const;

interface Draft {
  name: string;
  description: string;
  metric: MetricStat;
  expression: string;
  periodSec: number;
  comparison: string;
  threshold: string;
  m: string;
  n: string;
  treatMissingData: string;
  enabled: boolean;
  actions: Target[];
  /** 0 = evaluate the observed value */
  forecastHorizonSec: number;
}

type Errors = Partial<Record<"name" | "metric" | "expression" | "period" | "threshold" | "mn" | "comparison" | "missing", string>> & {
  actions?: (string | undefined)[];
};

const EMPTY_METRIC: MetricStat = { namespace: "", metricName: "", stat: "Average", dimensions: {} };

function fromRule(rule: AlarmRule | undefined, metric: MetricStat | undefined, forecastSec?: number): Draft {
  return {
    name: rule?.name ?? "",
    description: rule?.description ?? "",
    metric: rule?.metric ?? metric ?? EMPTY_METRIC,
    expression: rule?.expression ?? "",
    periodSec: rule?.periodSec || 60,
    comparison: rule?.config.comparison ?? "GreaterThanThreshold",
    threshold: rule ? String(rule.config.threshold) : "",
    m: String(rule?.config.datapointsToAlarm || rule?.config.evaluationPeriods || 3),
    n: String(rule?.config.evaluationPeriods || 3),
    treatMissingData: rule?.config.treatMissingData || "missing",
    enabled: rule?.enabled ?? true,
    actions: ((rule?.actions ?? []) as Target[]).map((a) => ({ ...a })),
    forecastHorizonSec: rule?.forecastHorizonSec ?? forecastSec ?? 0,
  };
}

function int(s: string): number | null {
  return /^\s*\d+\s*$/.test(s) ? Number(s) : null;
}

function validate(d: Draft): Errors {
  const e: Errors = {};
  const name = d.name.trim();
  if (!name) e.name = "A name is required.";
  else if (name.length > 255) e.name = `At most 255 characters (now ${name.length}).`;
  if (!d.metric.namespace || !d.metric.metricName) e.metric = "Pick a namespace and a metric.";
  if (d.expression.trim()) {
    let depth = 0;
    for (const ch of d.expression) {
      depth += ch === "(" ? 1 : ch === ")" ? -1 : 0;
      if (depth < 0) break;
    }
    if (depth !== 0) e.expression = "Parentheses do not balance.";
  }
  if (d.periodSec < 1 || d.periodSec > 86400) e.period = "The period must be between 1 s and 1 day.";
  else if (d.forecastHorizonSec && d.forecastHorizonSec < d.periodSec) e.period = "The forecast horizon must be at least one period.";
  if (d.threshold.trim() === "" || !Number.isFinite(Number(d.threshold))) e.threshold = "The threshold must be a number.";
  const n = int(d.n);
  const m = int(d.m);
  if (n == null || n < 1 || n > 64) e.mn = "N (evaluation periods) must be a whole number from 1 to 64.";
  else if (m == null || m < 1 || m > n) e.mn = `M (datapoints to alarm) must be a whole number from 1 to ${n}.`;
  if (!COMPARISONS.some((c) => c.id === d.comparison)) e.comparison = `Unknown comparison "${d.comparison}".`;
  if (!MISSING.some((x) => x.id === d.treatMissingData)) e.missing = `Unknown missing-data policy "${d.treatMissingData}".`;
  const actions = d.actions.map((a) => {
    if (a.type === "webhook" || a.type === "slack") return a.url?.trim() ? undefined : `A ${a.type} action needs a URL.`;
    if (a.type === "sns") return /^arn:aws:sns:[^:]+:[^:]*:[^:]+$/.test(a.url?.trim() ?? "") ? undefined : "An SNS action needs a topic ARN: arn:aws:sns:<region>:<account>:<topic>.";
    if (a.type === "pagerduty") return a.routingKey?.trim() ? undefined : "A PagerDuty action needs a routing key.";
    if (a.type === "log") return undefined;
    return `Unknown action type "${a.type}".`;
  });
  if (actions.some(Boolean)) e.actions = actions;
  return e;
}

function configOf(d: Draft): AlarmConfig {
  return {
    threshold: Number(d.threshold),
    comparison: d.comparison,
    evaluationPeriods: int(d.n) ?? 1,
    datapointsToAlarm: int(d.m) ?? 1,
    treatMissingData: d.treatMissingData,
  };
}

interface History {
  key: string;
  t: number[];
  v: (number | null)[];
  error?: string;
}

/** A tidy threshold: three significant figures. */
function tidy(v: number): number {
  return Number(v.toPrecision(3));
}

export function AlarmStudio({
  rule,
  initialMetric,
  initialForecastSec,
  onClose,
  onSaved,
  index = 0,
}: {
  /** The rule being edited; absent for a new one. */
  rule?: AlarmRule;
  /** Prefill for a new rule (deep link). */
  initialMetric?: MetricStat;
  /** Prefill a predictive rule (deep link from Explore's forecast). */
  initialForecastSec?: number;
  onClose: () => void;
  onSaved: (saved: AlarmRule) => void;
  index?: number;
}) {
  const [d, setD] = useState<Draft>(() => fromRule(rule, initialMetric, initialForecastSec));
  const set = (patch: Partial<Draft>) => setD((x) => ({ ...x, ...patch }));
  const [tried, setTried] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string>();
  const catalog = useSeriesCatalog();

  // Once the catalog arrives: a new rule without a deep link starts on the default metric, and a
  // deep-linked metric that leaves out dimension keys gets them as "*" (storage matches keys exactly).
  useEffect(() => {
    const cat = catalog.data;
    if (!cat?.length) return;
    if (!d.metric.namespace) {
      set({ metric: defaultMetric(cat) });
      return;
    }
    const keys = new Set(cat.filter((s) => s.namespace === d.metric.namespace && s.metricName === d.metric.metricName).flatMap((s) => Object.keys(s.dimensions ?? {})));
    const dims = d.metric.dimensions ?? {};
    const missing = [...keys].filter((k) => !(k in dims));
    if (missing.length) set({ metric: { ...d.metric, dimensions: { ...dims, ...Object.fromEntries(missing.map((k) => [k, "*"])) } } });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [catalog.data]);

  const errors = useMemo(() => validate(d), [d]);
  const valid = Object.keys(errors).length === 0;
  const config = configOf(d);
  const configOk = !errors.threshold && !errors.mn && !errors.comparison && !errors.missing;
  /** Name and action errors wait for a save attempt; numbers are checked as they are typed. */
  const show = (k: Exclude<keyof Errors, "actions">) => (k === "name" ? tried : true) && errors[k];

  // ---- 24 h of history at the chosen period ----
  const expression = d.expression.trim();
  const histKey = useDebounced(JSON.stringify({ metric: d.metric, expression: errors.expression ? "" : expression, period: d.periodSec }), 400);
  const [hist, setHist] = useState<History>();
  useEffect(() => {
    const { metric, expression: expr, period } = JSON.parse(histKey) as { metric: MetricStat; expression: string; period: number };
    if (!metric.namespace || !metric.metricName) {
      setHist(undefined);
      return;
    }
    const ctrl = new AbortController();
    const p = period * 1000;
    const end = Math.floor(Date.now() / p) * p;
    const start = end - DAY;
    const queries = expr
      ? [
          { id: "m1", metricStat: metric, returnData: false },
          { id: "e1", expression: expr },
        ]
      : [{ id: "m1", metricStat: metric }];
    const t = Array.from({ length: Math.round(DAY / p) }, (_, i) => start + i * p);
    endpoints
      .query({ queries, start, end, period }, ctrl.signal)
      .then((res) => {
        const r = res.find((x) => x.id === (expr ? "e1" : "m1"));
        const byT = new Map<number, number>();
        r?.timestamps.forEach((ts, i) => {
          const v = r.values[i];
          if (v != null && Number.isFinite(v)) byT.set(Math.floor(ts / p) * p, v);
        });
        const error = !r ? "the query returned no series" : r.statusCode === "InternalError" ? r.messages?.join("; ") || "the query failed" : undefined;
        setHist({ key: histKey, t, v: t.map((x) => byT.get(x) ?? null), error });
      })
      .catch((e: unknown) => {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setHist({ key: histKey, t: [], v: [], error: e instanceof Error ? e.message : String(e) });
      });
    return () => ctrl.abort();
  }, [histKey]);

  // ---- simulation, debounced, latest answer wins ----
  const simInput = JSON.stringify({ config, hist: hist?.key ?? "" });
  const simKey = useDebounced(simInput, 300);
  const [sim, setSim] = useState<SimulationResult & { key: string }>();
  const [simError, setSimError] = useState<string>();
  const seq = useRef(0);
  useEffect(() => {
    const my = ++seq.current;
    if (!hist || !hist.v.length || !configOk) {
      setSim(undefined);
      setSimError(undefined);
      return;
    }
    const { config: cfg } = JSON.parse(simKey) as { config: AlarmConfig };
    endpoints
      .simulateAlarm(cfg, hist.v)
      .then((r) => {
        if (my !== seq.current) return;
        setSim({ ...r, states: r.states ?? [], key: simKey });
        setSimError(undefined);
      })
      .catch((e: unknown) => {
        if (my !== seq.current) return;
        setSimError(e instanceof Error ? e.message : String(e));
      });
    // hist and configOk are read as of the render that changed simKey
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [simKey]);
  const simPending = !!sim && sim.key !== simInput;

  // ---- the current forecast, for predictive rules ----
  const fcKey = useDebounced(
    d.forecastHorizonSec && configOk && !errors.expression && !errors.period && d.metric.namespace
      ? JSON.stringify({ metric: d.metric, expression, period: d.periodSec, horizon: d.forecastHorizonSec, threshold: config.threshold, comparison: config.comparison })
      : "",
    400,
  );
  const [fc, setFc] = useState<{ key: string; res?: ForecastResult; error?: string }>();
  useEffect(() => {
    if (!fcKey) {
      setFc(undefined);
      return;
    }
    const q = JSON.parse(fcKey) as { metric: MetricStat; expression: string; period: number; horizon: number; threshold: number; comparison: string };
    const ctrl = new AbortController();
    endpoints
      .forecast(
        {
          metricStat: q.metric,
          ...(q.expression ? { expression: q.expression } : {}),
          period: q.period,
          horizon: Math.max(1, Math.ceil(q.horizon / q.period)),
          threshold: q.threshold,
          comparison: q.comparison,
        },
        ctrl.signal,
      )
      .then((res) => setFc({ key: fcKey, res }))
      .catch((e: unknown) => {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setFc({ key: fcKey, error: e instanceof Error ? e.message : String(e) });
      });
    return () => ctrl.abort();
  }, [fcKey]);
  const forecast = d.forecastHorizonSec && fc?.res ? fc.res : undefined;

  // ---- predictive rules: replay the forecast over the last 24 h (POST /alarms/backtest) ----
  const predictive = d.forecastHorizonSec > 0;
  const btKey = useDebounced(
    predictive && configOk && !errors.expression && !errors.period && d.metric.namespace
      ? JSON.stringify({ metric: d.metric, expression, periodSec: d.periodSec, forecastHorizonSec: d.forecastHorizonSec, config })
      : "",
    500,
  );
  const [bt, setBt] = useState<{ key: string; res?: BacktestResult; error?: string }>();
  useEffect(() => {
    if (!btKey) {
      setBt(undefined);
      return;
    }
    const q = JSON.parse(btKey) as { metric: MetricStat; expression: string; periodSec: number; forecastHorizonSec: number; config: AlarmConfig };
    const ctrl = new AbortController();
    endpoints
      .backtestAlarm({ metric: q.metric, expression: q.expression || undefined, periodSec: q.periodSec, forecastHorizonSec: q.forecastHorizonSec, config: q.config }, 24, ctrl.signal)
      .then((res) => setBt({ key: btKey, res: { ...res, predicted: { ...res.predicted, states: res.predicted.states ?? [] }, observed: { ...res.observed, states: res.observed.states ?? [] } } }))
      .catch((e: unknown) => {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setBt({ key: btKey, error: e instanceof Error ? e.message : String(e) });
      });
    return () => ctrl.abort();
  }, [btKey]);
  const btPending = !!btKey && bt?.key !== btKey;

  // ---- threshold suggestions from the history ----
  const present = useMemo(() => (hist?.v ?? []).filter((v): v is number => v != null).sort((a, b) => a - b), [hist]);
  const p99 = present.length ? present[Math.max(0, Math.ceil(0.99 * present.length) - 1)] : null;
  const mean = present.length ? present.reduce((a, b) => a + b, 0) / present.length : null;

  const save = async () => {
    setTried(true);
    setSaveError(undefined);
    if (!valid) return;
    setSaving(true);
    try {
      const saved = await endpoints.saveAlarm({
        ...(rule?.id ? { id: rule.id } : {}),
        name: d.name.trim(),
        description: d.description.trim() || undefined,
        metric: d.metric,
        expression: expression || undefined,
        periodSec: d.periodSec,
        config,
        actions: d.actions.map((a) =>
          a.type === "pagerduty"
            ? { type: a.type, routingKey: a.routingKey?.trim() }
            : a.type === "log"
              ? { type: a.type }
              : { type: a.type, url: a.url?.trim() },
        ),
        enabled: d.enabled,
        forecastHorizonSec: d.forecastHorizonSec,
      });
      onSaved(saved);
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  const unit = d.metric.metricName === "Latency" ? "ms" : undefined;
  const threshold = Number(d.threshold);
  const metricLabel = `${d.metric.stat} ${d.metric.metricName || "metric"}${expression ? ` → ${expression}` : ""}`;
  const periodLabel = PERIODS.find((p) => p.sec === d.periodSec)?.label ?? `${num(d.periodSec)} s`;

  return (
    <div className="rise-in border border-border" style={stagger(index)} role="region" aria-label={rule ? `Edit alarm ${rule.name}` : "New alarm"}>
      <div className="flex flex-wrap items-baseline justify-between gap-x-5 gap-y-2 border-b border-border px-4 py-3 sm:px-5">
        <h3 className={typography.micro}>{rule ? `Alarm studio · editing ${rule.name}` : "Alarm studio · new alarm"}</h3>
        <span className={typography.eyebrow}>simulated on the last 24 h before you save</span>
      </div>

      <div className="grid min-w-0 gap-x-8 gap-y-8 p-4 sm:p-5 xl:grid-cols-[minmax(0,26rem)_minmax(0,1fr)]">
        {/* ---- the rule ---- */}
        <div className="min-w-0 space-y-5">
          <Field label="Name" error={show("name")}>
            <input
              className={`${control.input} w-full`}
              value={d.name}
              maxLength={300}
              onChange={(e) => set({ name: e.target.value })}
              placeholder="checkout p99 latency"
            />
          </Field>
          <Field label="Description" hint="optional">
            <textarea
              className={`${control.input} block min-h-16 w-full resize-y`}
              value={d.description}
              onChange={(e) => set({ description: e.target.value })}
            />
          </Field>
          <Field label="Metric (m1)" error={show("metric")}>
            {catalog.data ? (
              <div className="xl:[&>div]:grid-cols-2">
                <MetricPicker value={d.metric} onChange={(metric) => set({ metric })} catalog={catalog.data} />
              </div>
            ) : catalog.error ? (
              <p className="text-[0.95rem] text-muted-foreground">Could not load the metric catalog: {catalog.error.message}</p>
            ) : (
              <Skeleton className="h-20 w-full" />
            )}
          </Field>
          <Field label="Expression" hint="optional · applied to m1, e.g. m1 / PERIOD(m1) for a per-second rate" error={show("expression")}>
            <input
              className={`${control.input} w-full`}
              value={d.expression}
              onChange={(e) => set({ expression: e.target.value })}
              placeholder="m1"
              spellCheck={false}
            />
          </Field>
          <Field label="Period">
            <Chips
              options={PERIODS.map((p) => ({ id: String(p.sec), label: p.label }))}
              value={String(d.periodSec)}
              onChange={(v) => set({ periodSec: Number(v) })}
            />
          </Field>
          <Field label="Evaluate" error={show("period")}>
            <Chips
              options={[
                { id: "observed", label: "Observed value" },
                { id: "forecast", label: "Forecast" },
              ]}
              value={d.forecastHorizonSec ? "forecast" : "observed"}
              onChange={(v) => set({ forecastHorizonSec: v === "forecast" ? (HORIZONS.find((h) => h.sec >= Math.max(3600, d.periodSec))?.sec ?? 86400) : 0 })}
            />
            {d.forecastHorizonSec > 0 && (
              <div className="mt-3 space-y-2">
                <div className={typography.eyebrow}>
                  <span className="text-foreground">Forecast for the next</span>
                </div>
                <Chips
                  options={HORIZONS.filter((h) => h.sec >= d.periodSec).map((h) => ({ id: String(h.sec), label: h.label }))}
                  value={String(d.forecastHorizonSec)}
                  onChange={(v) => set({ forecastHorizonSec: Number(v) })}
                />
                <p className="text-[0.9rem] leading-snug text-muted-foreground">
                  Each period the worst forecast value in that window is compared with the threshold. Uses the model service when one is configured,
                  otherwise the built-in forecast (picked by a holdout test). Too little history counts as missing data.
                </p>
              </div>
            )}
          </Field>
          <Field label={d.forecastHorizonSec ? "Alarm when the forecast is" : "Alarm when the value is"} error={show("comparison")}>
            <Chips
              options={COMPARISONS.map((c) => ({ id: c.id, label: `${compareSign(c.id)} ${c.words}` }))}
              value={d.comparison}
              onChange={(comparison) => set({ comparison })}
            />
          </Field>
          <Field label="Threshold" hint={unit} error={show("threshold")}>
            <div className="flex flex-wrap items-center gap-2">
              <input
                className={`${control.input} w-36 tabular-nums`}
                inputMode="decimal"
                value={d.threshold}
                onChange={(e) => set({ threshold: e.target.value })}
                placeholder="0"
              />
              <button type="button" className={control.button} disabled={p99 == null} onClick={() => p99 != null && set({ threshold: String(tidy(p99)) })}>
                p99 of history{p99 != null ? ` · ${num(tidy(p99), p99 < 10 ? 2 : 0)}` : ""}
              </button>
              <button
                type="button"
                className={control.button}
                disabled={mean == null}
                onClick={() => mean != null && set({ threshold: String(tidy(2 * mean)) })}
              >
                2× mean{mean != null ? ` · ${num(tidy(2 * mean), mean < 5 ? 2 : 0)}` : ""}
              </button>
            </div>
          </Field>
          <Field label="Datapoints to alarm" hint="M of N periods must breach" error={show("mn")}>
            <div className={`${typography.micro} flex flex-wrap items-center gap-2`}>
              <input
                className={`${control.input} w-16 tabular-nums`}
                inputMode="numeric"
                aria-label="M, datapoints to alarm"
                value={d.m}
                onChange={(e) => set({ m: e.target.value })}
              />
              <span>of</span>
              <input
                className={`${control.input} w-16 tabular-nums`}
                inputMode="numeric"
                aria-label="N, evaluation periods"
                value={d.n}
                onChange={(e) => set({ n: e.target.value })}
              />
              <span>periods of {periodLabel}</span>
            </div>
          </Field>
          <Field label="Treat missing data as" error={show("missing")}>
            <div className="border-b border-hair">
              {MISSING.map((x) => (
                <label key={x.id} className="flex cursor-pointer gap-3 border-t border-hair py-2.5">
                  <input
                    type="radio"
                    name={`missing-${rule?.id ?? "new"}`}
                    className="mt-0.5 accent-[var(--color-foreground)]"
                    checked={d.treatMissingData === x.id}
                    onChange={() => set({ treatMissingData: x.id })}
                  />
                  <span className="min-w-0">
                    <span className={`${typography.micro} block`}>{x.words}</span>
                    <span className="block text-[0.9rem] leading-snug text-muted-foreground">{x.explain}</span>
                  </span>
                </label>
              ))}
            </div>
          </Field>
          <label className={`${typography.micro} flex cursor-pointer items-center gap-3`}>
            <input
              type="checkbox"
              className="h-4 w-4 accent-[var(--color-foreground)]"
              checked={d.enabled}
              onChange={(e) => set({ enabled: e.target.checked })}
            />
            Enabled {d.enabled ? "· evaluated every period" : "· saved but not evaluated"}
          </label>
          <Field label="Actions" hint="sent on every state change">
            <div className="space-y-3">
              {d.actions.length === 0 && <p className="text-[0.9rem] text-muted-foreground">No actions: transitions are only recorded in the history.</p>}
              {d.actions.map((a, i) => {
                const patch = (p: Partial<Target>) => set({ actions: d.actions.map((x, j) => (j === i ? { ...x, ...p } : x)) });
                const err = tried ? errors.actions?.[i] : undefined;
                return (
                  <div key={i} className="border-t border-hair pt-3">
                    <div className="grid gap-2 sm:grid-cols-[9rem_minmax(0,1fr)_auto]">
                      <span className="relative block">
                        <select className={control.select} aria-label={`Action ${i + 1} type`} value={a.type} onChange={(e) => patch({ type: e.target.value })}>
                          {ACTION_TYPES.map((t) => (
                            <option key={t.id} value={t.id}>
                              {t.label}
                            </option>
                          ))}
                        </select>
                        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[0.6rem]" aria-hidden="true">
                          ▼
                        </span>
                      </span>
                      {a.type === "pagerduty" ? (
                        <input
                          className={control.input}
                          aria-label="PagerDuty routing key"
                          placeholder="routing key"
                          value={a.routingKey ?? ""}
                          onChange={(e) => patch({ routingKey: e.target.value })}
                          spellCheck={false}
                        />
                      ) : a.type === "log" ? (
                        <span className="self-center text-[0.9rem] text-muted-foreground">Writes the transition to the server log.</span>
                      ) : (
                        <input
                          className={control.input}
                          aria-label={a.type === "sns" ? "SNS topic ARN" : `${a.type} URL`}
                          type={a.type === "sns" ? "text" : "url"}
                          placeholder={a.type === "slack" ? "https://hooks.slack.com/services/…" : a.type === "sns" ? "arn:aws:sns:us-east-1:123456789012:alerts" : "https://example.com/hook"}
                          value={a.url ?? ""}
                          onChange={(e) => patch({ url: e.target.value })}
                          spellCheck={false}
                        />
                      )}
                      <button type="button" className={control.button} onClick={() => set({ actions: d.actions.filter((_, j) => j !== i) })}>
                        Remove
                      </button>
                    </div>
                    {err && <FieldError>{err}</FieldError>}
                  </div>
                );
              })}
              <button type="button" className={control.button} onClick={() => set({ actions: [...d.actions, { type: "webhook", url: "" }] })}>
                + Add action
              </button>
            </div>
          </Field>
        </div>

        {/* ---- the simulator ---- */}
        <div className="min-w-0 space-y-4 xl:sticky xl:top-20 xl:self-start">
          <div className="flex flex-wrap items-baseline justify-between gap-x-5 gap-y-1 border-b border-border pb-2.5">
            <h4 className={typography.micro}>Threshold simulator · last 24 h</h4>
            <span className={typography.eyebrow}>
              {metricLabel} {compareSign(d.comparison)} {errors.threshold ? "?" : num(threshold, Math.abs(threshold) < 10 ? 2 : 0)} ·{" "}
              {errors.mn ? "?" : `${d.m} of ${d.n}`} × {periodLabel}
            </span>
          </div>
          {!d.metric.namespace ? (
            <Skeleton className="h-56 w-full" />
          ) : !hist ? (
            <Skeleton className="h-56 w-full" />
          ) : hist.error ? (
            <div className="border border-dashed border-border p-5 text-[0.95rem] leading-relaxed">
              Could not load the history: <span className="font-mono text-[0.8rem]">{hist.error}</span>
              {expression ? ". Check the expression; it may only refer to m1." : "."}
            </div>
          ) : (
            <div>
              <MultiPlot
                series={[{ label: expression ? "e1" : d.metric.metricName, t: hist.t, v: hist.v }]}
                threshold={configOk && Number.isFinite(threshold) ? threshold : undefined}
                forecast={
                  forecast
                    ? {
                        t: forecast.points.map((p) => p.t),
                        forecast: forecast.points.map((p) => p.forecast),
                        lower: forecast.points.map((p) => p.lower),
                        upper: forecast.points.map((p) => p.upper),
                        crossesAt: forecast.crossesAt,
                      }
                    : undefined
                }
                unit={unit}
                height={240}
                ariaLabel={`${metricLabel}, last 24 hours, with the threshold`}
              />
              {predictive ? (
                <>
                  <StateStrip states={bt?.res?.predicted.states ?? []} pending={btPending || !bt} />
                  <StateStrip states={sim?.states ?? []} pending={simPending || (!sim && configOk)} thin />
                  <p className={`${typography.eyebrow} mt-1.5`}>
                    upper strip: the forecast rule replayed{bt?.res && bt.res.stride > 1 ? `, evaluated every ${plural(bt.res.stride, "period")}` : ""} · lower strip: the same threshold on observed values
                  </p>
                </>
              ) : (
                <StateStrip states={sim?.states ?? []} pending={simPending || (!sim && configOk)} />
              )}
              <PlotLegend
                labels={[expression ? `e1 = ${expression}` : metricLabel]}
                threshold={`threshold ${compareSign(d.comparison)} ${num(threshold, Math.abs(threshold) < 10 ? 2 : 0)}`}
                forecast={forecast ? `forecast · ${forecast.method}` : undefined}
              />
              {d.forecastHorizonSec > 0 && <ForecastNote fc={fc} pending={!!fcKey && fc?.key !== fcKey} horizon={d.forecastHorizonSec} />}
            </div>
          )}

          {hist && !hist.error && (
            <>
              {!configOk ? (
                <p className="border border-dashed border-border p-4 text-[0.95rem]">Fix the threshold and M of N to run the simulation.</p>
              ) : predictive ? (
                bt?.error ? (
                  <p className="border border-dashed border-border p-4 text-[0.95rem]">
                    Replay failed: <span className="font-mono text-[0.8rem]">{bt.error}</span>
                  </p>
                ) : bt?.res ? (
                  <SimSummary sim={bt.res.predicted} pending={btPending} lead={leadSentence(bt.res, sim?.alarmCount ?? bt.res.observed.alarmCount)} />
                ) : (
                  <Skeleton className="h-28 w-full" />
                )
              ) : simError ? (
                <p className="border border-dashed border-border p-4 text-[0.95rem]">
                  Simulation failed: <span className="font-mono text-[0.8rem]">{simError}</span>
                </p>
              ) : sim ? (
                <SimSummary sim={sim} pending={simPending} />
              ) : (
                <Skeleton className="h-28 w-full" />
              )}
            </>
          )}
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3 border-t border-border px-4 py-3 sm:px-5">
        <button type="button" className={control.buttonActive} onClick={() => void save()} disabled={saving}>
          {saving ? "Saving…" : rule ? "Save changes" : "Create alarm"}
        </button>
        <button type="button" className={control.button} onClick={onClose} disabled={saving}>
          Cancel
        </button>
        {tried && !valid && <span className="text-[0.9rem] text-tone-critical">Fix the fields marked above, then save again.</span>}
        {saveError && (
          <span className="text-[0.9rem]">
            <span className="text-tone-critical">Not saved:</span> <span className="font-mono text-[0.8rem]">{saveError}</span>
          </span>
        )}
      </div>
    </div>
  );
}

function ForecastNote({ fc, pending, horizon }: { fc?: { res?: ForecastResult; error?: string }; pending: boolean; horizon: number }) {
  const span = HORIZONS.find((h) => h.sec === horizon)?.label ?? `${num(horizon / 60)} min`;
  if (fc?.error)
    return (
      <p className="mt-3 border border-dashed border-border p-4 text-[0.95rem]">
        No forecast: <span className="font-mono text-[0.8rem]">{fc.error}</span>
      </p>
    );
  if (!fc?.res) return <Skeleton className="mt-3 h-10 w-full" />;
  const { res } = fc;
  return (
    <div className={`mt-3 space-y-1.5 transition-opacity ${pending ? "opacity-60" : ""}`} aria-live="polite">
      <p className="text-[0.95rem]">
        {res.crossesAt != null ? (
          <>
            <span className="mr-2 inline-block h-2.5 w-2.5 bg-tone-warning" aria-hidden="true" />
            <b>The forecast crosses the threshold at {clock(res.crossesAt).slice(0, 5)}</b>, so this period counts as breaching.
          </>
        ) : (
          <>The forecast stays on the safe side of the threshold for the next {span}.</>
        )}
      </p>
      <p className={typography.eyebrow}>
        method {res.method}
        {res.fallback ? ` · ${res.fallback}` : ""} · the replay below uses the built-in forecast
      </p>
    </div>
  );
}

function Field({ label, hint, error, children }: { label: string; hint?: string; error?: string | false; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <div className={`${typography.eyebrow} mb-1.5`}>
        <span className="text-foreground">{label}</span>
        {hint && <span> · {hint}</span>}
      </div>
      {children}
      {error && <FieldError>{error}</FieldError>}
    </div>
  );
}

function FieldError({ children }: { children: ReactNode }) {
  return (
    <p className="mt-1.5 text-[0.85rem] leading-snug text-tone-critical" role="alert">
      {children}
    </p>
  );
}

function Chips({ options, value, onChange }: { options: { id: string; label: string }[]; value: string; onChange: (id: string) => void }) {
  return (
    <div className="flex flex-wrap gap-1.5" role="radiogroup">
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          role="radio"
          aria-checked={o.id === value}
          className={o.id === value ? control.buttonActive : control.button}
          onClick={() => onChange(o.id)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

/** Width of the strip's own coordinate space; it spans the plot's time axis only. */
const STRIP_W = 1000;
/** Up to this many periods each gets its own square; beyond, equal neighbours merge into runs. */
const MAX_CELLS = 96;

/**
 * The simulated state per period, aligned under the plot's time axis (same
 * pixel margins as MultiPlot's plot area): filled = ALARM, outlined = OK,
 * dashed = insufficient data.
 */
function StateStrip({ states, pending, thin }: { states: AlarmState[]; pending: boolean; thin?: boolean }) {
  const count = states.length;
  const span = STRIP_W;
  const step = count > 1 ? span / (count - 1) : span;
  const x = (i: number) => (count > 1 ? i * step : span / 2);

  const segs: { from: number; to: number; state: AlarmState }[] = [];
  states.forEach((s, i) => {
    const last = segs[segs.length - 1];
    if (count > MAX_CELLS && last && last.state === s) last.to = i;
    else segs.push({ from: i, to: i, state: s });
  });
  const gap = count <= MAX_CELLS ? Math.min(2, step * 0.25) : 0;

  return (
    <div
      className={`border-x border-b border-border transition-opacity ${pending ? "opacity-50" : ""}`}
      style={{ paddingLeft: PLOT_PAD.l, paddingRight: PLOT_PAD.r }}
    >
      <svg viewBox={`0 0 ${STRIP_W} 14`} preserveAspectRatio="none" className={`wipe block w-full ${thin ? "h-2 opacity-70" : "h-3.5"}`} role="img" aria-label={stripLabel(states)}>
        {segs.map((s, k) => {
          const x0 = Math.max(0, x(s.from) - step / 2 + gap / 2);
          const x1 = Math.min(STRIP_W, x(s.to) + step / 2 - gap / 2);
          const common = { x: x0, y: 2.5, width: Math.max(0.5, x1 - x0), height: 9, vectorEffect: "non-scaling-stroke" as const };
          return s.state === "ALARM" ? (
            <rect key={k} {...common} className="fill-tone-critical" />
          ) : s.state === "OK" ? (
            <rect key={k} {...common} className="fill-none stroke-foreground/45" strokeWidth={1} />
          ) : (
            <rect key={k} {...common} className="fill-none stroke-muted-foreground" strokeWidth={1} strokeDasharray="2 2" />
          );
        })}
      </svg>
    </div>
  );
}

function stripLabel(states: AlarmState[]): string {
  if (!states.length) return "Simulated states loading";
  const a = states.filter((s) => s === "ALARM").length;
  const o = states.filter((s) => s === "OK").length;
  return `Simulated state over ${states.length} periods: ${a} in alarm, ${o} OK, ${states.length - a - o} insufficient data`;
}

/** What the replay says against the observed rule: fired earlier, later, or only one of them. */
function leadSentence(bt: BacktestResult, observedAlarms: number): string {
  const p = bt.predicted.alarmCount;
  if (bt.leadSec != null) {
    const m = Math.round(Math.abs(bt.leadSec) / 60);
    const span = m >= 120 ? `${num(m / 60, 1)} h` : plural(m, "minute");
    return bt.leadSec > 0 ? `First alarm ${span} before the observed value crossed.` : bt.leadSec < 0 ? `First alarm ${span} after the observed value crossed.` : "First alarm at the same time as on observed values.";
  }
  if (p > 0 && observedAlarms === 0) return "The observed values never crossed: these would have been early warnings, or false alarms.";
  if (p === 0 && observedAlarms > 0) return "The observed values crossed, but the forecast did not see it coming.";
  return "";
}

function SimSummary({ sim, pending, lead }: { sim: SimulationResult; pending: boolean; lead?: string }) {
  const states = sim.states ?? [];
  const alarm = states.filter((s) => s === "ALARM").length;
  const ok = states.filter((s) => s === "OK").length;
  const none = states.length - alarm - ok;
  return (
    <div className={`space-y-4 transition-opacity ${pending ? "opacity-60" : ""}`} aria-live="polite">
      <p className={typography.sectionTitle}>
        {sim.alarmCount === 0 ? "Would not have fired in 24 h." : `Would have fired ${sim.alarmCount === 1 ? "once" : `${num(sim.alarmCount)} times`} in 24 h.`}
      </p>
      {lead && <p className="text-[0.95rem]">{lead}</p>}
      <StatGrid>
        <Stat label="Times into alarm" value={sim.alarmCount} unit="transitions to ALARM" />
        <Stat label="Breaching" value={sim.breachCount} unit={`of ${plural(states.length, "datapoint")}`} />
        <Stat label="Periods in alarm" value={alarm} unit={`of ${plural(states.length, "period")}`} />
        <Stat label="Periods OK" value={ok} unit={`${num(none)} insufficient data`} />
      </StatGrid>
      <div className={`${typography.micro} flex flex-wrap gap-x-5 gap-y-1.5`}>
        <span className="inline-flex items-center gap-2">
          <span className="inline-block h-2.5 w-2.5 bg-tone-critical" aria-hidden="true" />
          In alarm · {plural(alarm, "period")}
        </span>
        <span className="inline-flex items-center gap-2">
          <span className="inline-block h-2.5 w-2.5 border border-foreground/45" aria-hidden="true" />
          OK · {plural(ok, "period")}
        </span>
        <span className="inline-flex items-center gap-2 text-muted-foreground">
          <span className="hatch inline-block h-2.5 w-2.5 border border-dashed border-muted-foreground" aria-hidden="true" />
          Insufficient data · {plural(none, "period")}
        </span>
      </div>
    </div>
  );
}
