/**
 * Typed client for the ZeroWatch JSON API (internal/api). Shapes mirror the Go
 * structs' JSON tags. The API key, when the server has auth on, is kept in
 * localStorage and sent as X-API-Key (or ?api_key= for EventSource, which
 * cannot set headers).
 */

export interface LogEvent {
  logGroup: string;
  logStream: string;
  timestamp: number;
  message: string;
  level?: string;
  attributes?: Record<string, string>;
}

export interface LogResult {
  events: LogEvent[];
  scanned: number;
  truncated: boolean;
  /** why some lines may be missing (cluster shards that did not answer) */
  partial?: string;
}

/** A saved Log Explorer search (GET/POST /api/v1/logs/saved, DELETE /api/v1/logs/saved/{id}). */
export interface SavedQuery {
  id: string;
  name: string;
  query: string;
  logGroup?: string;
  createdAt: string;
}

export interface LogGroupInfo {
  name: string;
  streams: string[];
  events: number;
  lastEvent: number;
}

export interface LogQuery {
  logGroup?: string;
  level?: string;
  query?: string;
  start?: number;
  end?: number;
  limit?: number;
}

export interface SeriesMeta {
  id: string;
  namespace: string;
  metricName: string;
  dimensions: Record<string, string>;
  unit?: string;
}

export interface MetricStat {
  namespace: string;
  metricName: string;
  dimensions?: Record<string, string>;
  stat: string;
}

export interface MetricDataQuery {
  id: string;
  label?: string;
  expression?: string;
  metricStat?: MetricStat;
  returnData?: boolean;
}

export interface QueryRequest {
  queries: MetricDataQuery[];
  start: number;
  end: number;
  period: number;
  fill?: string;
}

export interface QueryResult {
  id: string;
  label: string;
  timestamps: number[];
  values: number[];
  statusCode: "Complete" | "PartialData" | "InternalError";
  messages?: string[];
}

export type AlarmState = "OK" | "ALARM" | "INSUFFICIENT_DATA";

export interface AlarmConfig {
  threshold: number;
  comparison: string;
  evaluationPeriods: number;
  datapointsToAlarm: number;
  treatMissingData: string;
}

export interface AlarmRule {
  id: string;
  name: string;
  description?: string;
  metric: MetricStat | null;
  expression?: string;
  periodSec: number;
  /** Predictive alarm: evaluate the worst forecast over the next N seconds instead of the observed value (0/absent = off). */
  forecastHorizonSec?: number;
  config: AlarmConfig;
  /** Notification targets (internal/alarms/notify.go Target): webhook/slack need url, pagerduty needs routingKey. */
  actions?: { type: string; url?: string; routingKey?: string }[];
  enabled: boolean;
  state: AlarmState | "";
  updatedAt: string;
}

export interface AlarmTransition {
  ruleId: string;
  ruleName: string;
  from: AlarmState | "";
  to: AlarmState;
  reason: string;
  value?: number;
  timestamp: string;
}

export interface Process {
  pid: number;
  ppid: number;
  comm: string;
  cmdline: string;
  kind?: string;
  rssKb: number;
  threads: number;
  ports?: number[];
}

export interface Host {
  hostname: string;
  os: string;
  arch: string;
  kernel?: string;
  cpus: number;
  btf: boolean;
  ebpfCapable: boolean;
  inContainer: boolean;
  inKubernetes: boolean;
  cloud?: string;
  dockerSocket?: string;
  containerdSocket?: string;
  systemd: boolean;
}

export interface Container {
  id: string;
  name: string;
  image: string;
  state: string;
  ports?: number[];
}

export interface ProbeStatus {
  name: string;
  mode: string;
  healthy: boolean;
  targets: number;
  events: number;
  emitted: number;
  lastEmit: string;
  error?: string;
}

export interface LogSource {
  path: string;
  size: number;
  logGroup: string;
  guess: { format: string; confidence: number; sampled: number };
  offset: number;
  shipped: number;
  lastSeen: string;
  error?: string;
}

/**
 * Who talks to whom, from the host's TCP table (supervisor, every cycle).
 * Node ids of local services equal the probe's RED "Service" dimension, so
 * RED metrics join onto nodes by id. External peers are "ext:<ip>" (or
 * "ext:other" once too many distinct peers are seen).
 */
export interface TopoNode {
  id: string;
  label: string;
  kind: "service" | "external";
  pid?: number;
  ports?: number[];
}

export interface TopoEdge {
  source: string;
  target: string;
  port: number;
  connections: number;
}

export interface Topology {
  nodes: TopoNode[] | null;
  edges: TopoEdge[] | null;
}

export interface Widget {
  id?: string;
  type: "timeseries" | "stat" | "logs" | "alarm" | "text" | "anomaly" | "forecast";
  title: string;
  /**
   * timeseries / stat / anomaly: { queries: MetricDataQuery[], period } (anomaly bands the first returned series);
   * forecast: the same plus { horizonMin, threshold? } (forecasts the first returned series, says when it crosses the threshold);
   * logs: LogQuery; text: { text }; alarm: { service?: string } (rules whose metric Service matches, all when absent).
   */
  query?: unknown;
  visual?: unknown;
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface Dashboard {
  id?: string;
  title: string;
  description?: string;
  /** { autogen?, schema?, range?: "15m"|"1h"|"6h"|"24h", refresh?: seconds } */
  layout?: { autogen?: boolean; schema?: number; range?: string; refresh?: number } | null;
  widgets: Widget[];
  version?: number;
  updatedAt?: string;
}

export interface DashboardSummary {
  id: string;
  title: string;
  widgets: number;
  updatedAt: string;
}

/** One point of a Holt-Winters band; null while the model warms up (first season). */
export interface AnomalyBand {
  forecast: number | null;
  lower: number | null;
  upper: number | null;
  actual: number | null;
  anomalous: boolean;
}

export interface SimulationResult {
  states: AlarmState[];
  alarmCount: number;
  breachCount: number;
}

/**
 * POST /api/v1/alarms/backtest {rule, hours?}: a predictive rule replayed over
 * the last hours (default 24) with the built-in forecast, evaluated every
 * `stride` periods; `observed` is the same rule on the observed values.
 */
export interface BacktestResult {
  forecast: (number | null)[];
  predicted: SimulationResult;
  observed: SimulationResult;
  /** first predicted ALARM this many seconds before the first observed one (negative: after); null unless both fired */
  leadSec: number | null;
  stride: number;
  start: number;
  method: string;
}

/**
 * Distributed traces (OTLP). Times are unix milliseconds (fractional allowed).
 * GET /api/v1/traces?service=&name=&minDurationMs=&error=true&start=&end=&limit= → { traces: TraceSummary[] } (newest first)
 * GET /api/v1/traces/{traceId} → TraceDetail (404 when unknown)
 */
export interface TraceSummary {
  traceId: string;
  rootService: string;
  rootName: string;
  start: number;
  durationMs: number;
  spans: number;
  errors: number;
  services: string[];
}

export interface Span {
  traceId: string;
  spanId: string;
  parentSpanId?: string;
  service: string;
  name: string;
  /** "server" | "client" | "internal" | "producer" | "consumer" */
  kind: string;
  start: number;
  durationMs: number;
  status: "ok" | "error" | "unset";
  statusMessage?: string;
  attributes?: Record<string, string>;
}

export interface TraceDetail {
  traceId: string;
  spans: Span[];
}

export interface TraceQuery {
  service?: string;
  name?: string;
  minDurationMs?: number;
  error?: boolean;
  start?: number;
  end?: number;
  limit?: number;
}

/**
 * Forecasting. POST /api/v1/forecast with either a stored metric (metricStat
 * or metricStat+expression over [start,end) at period) or raw values (one per
 * period ending at `end`). method "auto" (default) = the optional model
 * service when configured, else built-in chosen by a holdout test; "holtwinters" | "stl" | "holt" | "model".
 * With threshold+comparison the response says when the forecast first breaches.
 */
export interface ForecastRequest {
  metricStat?: MetricStat;
  expression?: string;
  values?: (number | null)[];
  start?: number;
  end?: number;
  period: number;
  /** number of future periods */
  horizon: number;
  /** points per season; default derived from period (1 day) */
  season?: number;
  method?: "auto" | "holtwinters" | "stl" | "holt" | "model";
  /** central interval coverage for lower/upper, default 0.8 */
  interval?: number;
  threshold?: number;
  comparison?: string;
}

export interface ForecastPoint {
  t: number;
  forecast: number;
  lower: number;
  upper: number;
}

export interface ForecastResult {
  /** method actually used */
  method: string;
  /** set when "auto"/"model" fell back to a built-in method, with the reason */
  fallback?: string;
  history: { t: number; v: number | null }[];
  points: ForecastPoint[];
  /** first forecast timestamp breaching threshold (by forecast value), null if none in horizon */
  crossesAt?: number | null;
}

/**
 * Reports. GET /api/v1/reports/summary?range=24h|7d|30d → Report (JSON);
 * GET /api/v1/reports/summary.html?range=… → a self-contained HTML report;
 * POST /api/v1/reports/send {range} → {sent: string[]} to the configured targets.
 */
export interface Report {
  generatedAt: string;
  range: { id: string; start: number; end: number };
  totals: { requests: number; errorPct: number | null; p50: number | null; p99: number | null; availabilityPct: number | null };
  services: {
    name: string;
    requests: number;
    errorPct: number | null;
    p99: number | null;
    /** % change of p99 vs the previous period of the same length; null without data */
    p99ChangePct: number | null;
    availabilityPct: number | null;
  }[];
  alarms: { id: string; name: string; fired: number; minutesInAlarm: number; state: AlarmState | "" }[];
  anomalies: { service: string; metric: string; at: number; value: number; expectedLower: number; expectedUpper: number }[];
  slowTraces: TraceSummary[];
  capacity: { label: string; service?: string; metric: string; threshold: number; crossesAt: number | null; method: string }[];
  narrative?: string;
}

export interface SupervisorState {
  topology?: Topology;
  cycle: number;
  lastRun: string;
  host: Host;
  services: Process[] | null;
  containers?: Container[] | null;
  targets: { pid: number; port: number; service: string }[] | null;
  probe: ProbeStatus;
  logSources: LogSource[] | null;
  synthesized: string[] | null;
  watchdog: { supervisorCpuPct: number; processCpuPct: number; rssMb: number; sampleRate: number; throttled: boolean };
  decisions: string[] | null;
}

export interface Stats {
  storage: { backend: string; series: number; samples: number; chunkBytes: number; bytesPerPoint: number; rollupBuckets: number; logEvents: number };
  ingest: { metricsAccepted: number; metricsRejected: number; logsAccepted: number; logsRejected: number; emfExtracted: number };
  live: { subscribers: number; published: number; dropped: number };
  role?: string;
  node?: string;
  ebpfBuild?: boolean;
  commitLog?: { published: number; delivered: number; dropped: number; depth: number; capacity: number };
  supervisor?: SupervisorState;
}

const KEY_STORAGE = "zerowatch:api-key";

export function getApiKey(): string {
  try {
    return localStorage.getItem(KEY_STORAGE) ?? "";
  } catch {
    return "";
  }
}

export function setApiKey(key: string): void {
  try {
    if (key) localStorage.setItem(KEY_STORAGE, key);
    else localStorage.removeItem(KEY_STORAGE);
  } catch {
    // Without storage the key lasts until reload; acceptable.
  }
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

type AuthListener = () => void;
const authListeners = new Set<AuthListener>();
/** Called when the server answers 401, so the shell can ask for a key. */
export function onUnauthorized(fn: AuthListener): () => void {
  authListeners.add(fn);
  return () => authListeners.delete(fn);
}

export async function api<T>(path: string, init?: { method?: string; body?: unknown; signal?: AbortSignal }): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  const key = getApiKey();
  if (key) headers["X-API-Key"] = key;
  if (init?.body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, {
    method: init?.method ?? (init?.body !== undefined ? "POST" : "GET"),
    headers,
    body: init?.body !== undefined ? JSON.stringify(init.body) : undefined,
    signal: init?.signal,
  });
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`;
    try {
      const j = (await res.json()) as { error?: { message?: string } | string; message?: string };
      const e = typeof j.error === "string" ? j.error : j.error?.message;
      message = e ?? j.message ?? message;
    } catch {
      // Body was not JSON; keep the status line.
    }
    if (res.status === 401) authListeners.forEach((fn) => fn());
    throw new ApiError(res.status, message);
  }
  return (await res.json()) as T;
}

/** GET /api/v1/auth/me (internal/authn.Me). Public: answers "nobody" too. */
export interface Me {
  authenticated: boolean;
  /** Email, else subject; "" when nobody is signed in or auth is off. */
  user: string;
  sub?: string;
  role: "" | "viewer" | "editor" | "admin";
  tenant: number;
  method: "oidc" | "apikey" | "sigv4" | "none";
  /** Sign-in through /auth/login is available. */
  oidc: boolean;
}

/** Who is calling. A 404 (a server older than OIDC) reads as "auth unknown". */
export async function fetchMe(): Promise<Me | null> {
  try {
    const me = await api<Me>("/api/v1/auth/me");
    // A signed-in session makes a stored key redundant; a stale one would
    // override the cookie on every call.
    if (me.method === "oidc" && getApiKey()) setApiKey("");
    return me;
  } catch {
    return null;
  }
}

/** Where "Sign in" goes; returns to the current view afterwards. */
export function signInUrl(): string {
  return `/auth/login?return=${encodeURIComponent(`/${window.location.hash}`)}`;
}

export const SIGN_OUT_URL = "/auth/logout";

export function withKey(path: string): string {
  const key = getApiKey();
  if (!key) return path;
  return `${path}${path.includes("?") ? "&" : "?"}api_key=${encodeURIComponent(key)}`;
}

export const RED_NAMESPACE = "ZeroWatch/RED";

export const endpoints = {
  stats: () => api<Stats>("/api/v1/stats"),
  /** Empty namespace / metricName list every series (up to limit). */
  series: (namespace: string, metricName: string, limit = 5000) =>
    api<{ series: SeriesMeta[] }>(
      `/api/v1/series?namespace=${encodeURIComponent(namespace)}&metricName=${encodeURIComponent(metricName)}&limit=${limit}`,
    ).then((r) => r.series ?? []),
  dashboards: () => api<{ dashboards: DashboardSummary[] }>("/api/v1/dashboards").then((r) => r.dashboards ?? []),
  dashboard: (id: string) => api<Dashboard>(`/api/v1/dashboards/${encodeURIComponent(id)}`),
  /** Create (no id) or update (id + version, compare-and-swap: 409 when someone else saved first). */
  saveDashboard: (d: Dashboard) =>
    d.id
      ? api<Dashboard>(`/api/v1/dashboards/${encodeURIComponent(d.id)}`, { method: "PUT", body: d })
      : api<Dashboard>("/api/v1/dashboards", { method: "POST", body: d }),
  deleteDashboard: (id: string) => api<{ deleted: boolean }>(`/api/v1/dashboards/${encodeURIComponent(id)}`, { method: "DELETE" }),
  saveAlarm: (r: Partial<AlarmRule>) =>
    r.id
      ? api<AlarmRule>(`/api/v1/alarms/${encodeURIComponent(r.id)}`, { method: "PUT", body: r })
      : api<AlarmRule>("/api/v1/alarms", { method: "POST", body: r }),
  deleteAlarm: (id: string) => api<{ deleted: boolean }>(`/api/v1/alarms/${encodeURIComponent(id)}`, { method: "DELETE" }),
  /** Replays per-period values through a fresh alarm state machine. null = missing period. */
  simulateAlarm: (config: AlarmConfig, values: (number | null)[]) =>
    api<SimulationResult>("/api/v1/alarms/simulate", { body: { config, values } }),
  backtestAlarm: (rule: Partial<AlarmRule>, hours = 24, signal?: AbortSignal) =>
    api<BacktestResult>("/api/v1/alarms/backtest", { body: { rule, hours }, signal }),
  /**
   * Expected-range bands; season = points per season (min 2, default 60).
   * method "holtwinters" (default: online, first season null) or "stl"
   * (robust seasonal-trend decomposition, k=4: a band for every point, needs ≥ 2 seasons).
   */
  anomaly: (values: number[], season: number, signal?: AbortSignal, method: "holtwinters" | "stl" = "holtwinters") =>
    api<{ bands: AnomalyBand[] }>("/api/v1/anomaly", { body: { values, season, method }, signal }).then((r) => r.bands ?? []),
  query: (req: QueryRequest, signal?: AbortSignal) =>
    api<{ results: QueryResult[] }>("/api/v1/query", { body: req, signal }).then((r) => r.results ?? []),
  logGroups: () => api<{ logGroups: LogGroupInfo[] }>("/api/v1/logs/groups").then((r) => r.logGroups ?? []),
  savedQueries: () => api<{ queries: SavedQuery[] }>("/api/v1/logs/saved").then((r) => r.queries ?? []),
  saveQuery: (q: { name: string; query: string; logGroup?: string }) => api<SavedQuery>("/api/v1/logs/saved", { body: q }),
  deleteSavedQuery: (id: string) => api<{ deleted: boolean }>(`/api/v1/logs/saved/${encodeURIComponent(id)}`, { method: "DELETE" }),
  searchLogs: (q: LogQuery, signal?: AbortSignal) => api<LogResult>("/api/v1/logs/search", { body: q, signal }),
  alarms: () => api<{ alarms: AlarmRule[] }>("/api/v1/alarms").then((r) => r.alarms ?? []),
  alarmHistory: (id: string, limit = 5) =>
    api<{ history: AlarmTransition[] }>(`/api/v1/alarms/${encodeURIComponent(id)}/history?limit=${limit}`).then((r) => r.history ?? []),
  supervisor: () => api<SupervisorState>("/api/v1/supervisor"),
  traces: (q: TraceQuery, signal?: AbortSignal) => {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(q)) if (v !== undefined && v !== "" && v !== false) p.set(k, String(v));
    return api<{ traces: TraceSummary[] }>(`/api/v1/traces?${p.toString()}`, { signal }).then((r) => r.traces ?? []);
  },
  trace: (id: string, signal?: AbortSignal) => api<TraceDetail>(`/api/v1/traces/${encodeURIComponent(id)}`, { signal }),
  forecast: (req: ForecastRequest, signal?: AbortSignal) => api<ForecastResult>("/api/v1/forecast", { body: req, signal }),
  report: (range: string, signal?: AbortSignal) => api<Report>(`/api/v1/reports/summary?range=${encodeURIComponent(range)}`, { signal }),
  sendReport: (range: string) => api<{ sent: string[] }>("/api/v1/reports/send", { body: { range } }),
};
