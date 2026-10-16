// A stand-in for `zerowatch --role=all` so the UI can be built and checked
// without Go. It mimics the JSON shapes and semantics of internal/api:
// the synthetic generator (6 services × 3 routes, log-normal latency,
// incidents), exact dimension matching with "*" wildcards, per-period stats,
// log search with the server's term syntax, the SSE live tail, M-of-N alarms
// and a supervisor snapshot. It also serves web/dist with an SPA fallback.
//
//   node dev/mock-api.mjs            # http://localhost:8787
//   PORT=9000 node dev/mock-api.mjs
//   MOCK_AUTH=oidc node dev/mock-api.mjs   # fake OIDC sign-in (MOCK_ROLE=viewer|editor|admin; MOCK_SIGNED_IN=1 = always signed in)
//
// Not a test oracle: the Go handlers remain the source of truth.

import { createServer } from "node:http";
import { readFile, stat } from "node:fs/promises";
import { extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const PORT = Number(process.env.PORT ?? 8787);
const DIST = fileURLToPath(new URL("../dist", import.meta.url));
const NS = "ZeroWatch/RED";
const ROUTES = ["GET /", "POST /api", "GET /health"];
const ROUTE_W = [1, 0.6, 0.25];
const LAT_W = [1, 1.8, 0.1];
const ERR_MSGS = ["upstream timeout after 3000ms", "connection reset by peer", "database deadlock detected", "invalid token signature", "rate limit exceeded on dependency"];
const KEEP_MS = 35 * 60_000;

let seed = 42;
const rnd = () => ((seed = (seed * 1664525 + 1013904223) >>> 0) / 2 ** 32);
const gauss = () => Math.sqrt(-2 * Math.log(1 - rnd())) * Math.cos(2 * Math.PI * rnd());
const poisson = (l) => {
  if (l > 30) return Math.max(0, Math.round(l + Math.sqrt(l) * gauss()));
  const L = Math.exp(-l);
  let k = 0, p = 1;
  for (;;) { p *= rnd(); if (p <= L) return k; k++; }
};
const hex = () => Math.floor(rnd() * 2 ** 32).toString(16).padStart(8, "0");

const services = ["checkout", "payments", "catalog", "auth", "search", "inventory"].map((name) => ({
  name, baseRPS: 8 + rnd() * 30, baseLat: 15 + rnd() * 80, errRate: 0.002 + rnd() * 0.01, phase: rnd() * 2 * Math.PI, incidentTo: 0,
}));

/** Samples per series: { t, v } where Requests/Errors are counts and Latency are raw samples. */
const series = new Map(); // key -> { meta, pts: [{t,v}] }
const logs = []; // oldest first
const sse = new Set();
const decisions = []; // supervisor decision log, "15:04:05 message"
let metricsAccepted = 0, logsAccepted = 0, published = 0;

function put(metricName, dims, t, v) {
  const key = `${metricName}|${dims.Route}|${dims.Service}`;
  let s = series.get(key);
  if (!s) {
    s = { meta: { id: String(series.size + 1), tenantId: 1, namespace: NS, metricName, dimensions: dims, unit: metricName === "Latency" ? "Milliseconds" : "Count" }, pts: [] };
    series.set(key, s);
  }
  s.pts.push({ t, v });
  metricsAccepted++;
}

// ---- node metrics (internal/hostmetrics shape: namespace ZeroWatch/Host, Host dimension) ----
const HOST_NS = "ZeroWatch/Host";
const HOST = "zw-mock";
function putSeries(namespace, metricName, dims, t, v, unit) {
  const key = `${namespace}|${metricName}|${JSON.stringify(Object.entries(dims).sort())}`;
  let s = series.get(key);
  if (!s) {
    s = { meta: { id: String(series.size + 1), tenantId: 1, namespace, metricName, dimensions: dims, unit }, pts: [] };
    series.set(key, s);
  }
  s.pts.push({ t, v });
  metricsAccepted++;
}
let lastHostSample = 0;
const hostBoot = Date.now() - 30 * 60_000;
/** One sample every 10 s: daily-ish CPU, a slow memory leak and a filling root disk, so capacity forecasts have something to say. */
function sampleHost(now) {
  if (now - lastHostSample < 10_000) return;
  lastHostSample = now;
  const min = (now - hostBoot) / 60_000;
  const h = { Host: HOST };
  const cpu = Math.max(1, Math.min(99, 35 + 15 * Math.sin((2 * Math.PI * min) / 20) + 6 * gauss()));
  putSeries(HOST_NS, "CPUUtilization", h, now, cpu, "Percent");
  putSeries(HOST_NS, "CPUIOWait", h, now, Math.max(0, 2 + gauss()), "Percent");
  putSeries(HOST_NS, "CPUSteal", h, now, Math.max(0, 0.3 + 0.2 * gauss()), "Percent");
  putSeries(HOST_NS, "LoadAverage1", h, now, Math.max(0, (cpu / 100) * 4 + 0.2 * gauss()), "None");
  putSeries(HOST_NS, "MemoryUsedPercent", h, now, Math.min(99, 62 + 0.45 * min + 0.6 * gauss()), "Percent");
  putSeries(HOST_NS, "SwapUsedPercent", h, now, 3 + 0.05 * min, "Percent");
  putSeries(HOST_NS, "DiskUsedPercent", { ...h, Mount: "/" }, now, Math.min(100, 78 + 0.06 * min + 0.02 * gauss()), "Percent");
  putSeries(HOST_NS, "DiskUsedPercent", { ...h, Mount: "/data" }, now, 41 + 0.02 * gauss(), "Percent");
  putSeries(HOST_NS, "DiskReadBytes", { ...h, Device: "nvme0n1" }, now, Math.max(0, 4e5 + 1.5e5 * gauss()), "Bytes/Second");
  putSeries(HOST_NS, "DiskWriteBytes", { ...h, Device: "nvme0n1" }, now, Math.max(0, 9e5 + 3e5 * gauss()), "Bytes/Second");
  putSeries(HOST_NS, "NetworkRxBytes", { ...h, Interface: "eth0" }, now, Math.max(0, 2.5e6 + 8e5 * gauss()), "Bytes/Second");
  putSeries(HOST_NS, "NetworkTxBytes", { ...h, Interface: "eth0" }, now, Math.max(0, 1.2e6 + 4e5 * gauss()), "Bytes/Second");
}

function pushLog(e) {
  logs.push(e);
  logsAccepted++;
  const frame = `event: log\ndata: ${JSON.stringify({ kind: "log", time: e.timestamp, log: e })}\n\n`;
  for (const res of sse) res.write(frame);
  published++;
}

function tick(now, stepMs, withLogs) {
  sampleHost(now);
  const season = (2 * Math.PI * ((now / 1000) % 3600)) / 3600;
  for (const s of services) {
    const incident = now < s.incidentTo;
    ROUTES.forEach((route, ri) => {
      const rps = s.baseRPS * ROUTE_W[ri] * (1 + 0.35 * Math.sin(season + s.phase));
      const reqs = poisson((rps * stepMs) / 1000);
      let errRate = s.errRate, latMul = 1;
      if (incident && ri === 1) { errRate = 0.12 + rnd() * 0.1; latMul = 4 + rnd() * 3; }
      let errs = 0;
      for (let i = 0; i < reqs; i++) if (rnd() < errRate) errs++;
      const dims = { Route: route, Service: s.name };
      put("Requests", dims, now, reqs);
      put("Errors", dims, now, errs);
      const n = Math.min(reqs, 20);
      const offs = Array.from({ length: n }, () => -Math.floor(rnd() * stepMs)).sort((a, b) => a - b);
      for (let i = 0; i < n; i++) {
        const lat = Math.round(s.baseLat * LAT_W[ri] * Math.exp(0.55 * gauss()) * latMul * 100) / 100;
        put("Latency", dims, now + offs[i], lat);
        if (withLogs && (i % 4 === 0 || i < errs)) {
          const isErr = i < errs;
          let status = 200, level = "info", msg = `${route} completed`;
          if (isErr) { status = 500 + Math.floor(rnd() * 4); level = "error"; msg = ERR_MSGS[Math.floor(rnd() * ERR_MSGS.length)]; }
          else if (lat > 500) { level = "warn"; msg = `slow request ${route}`; }
          else if (rnd() < 0.02) { status = 404; msg = `${route} not found`; }
          const trace = hex() + hex() + hex() + hex();
          pushLog({
            tenantId: 1, logGroup: `/zerowatch/synthetic/${s.name}`, logStream: `${s.name}-pod-${Math.floor(rnd() * 3)}`, timestamp: now + offs[i], level,
            message: JSON.stringify({ ts: now + offs[i], level, service: s.name, route, status, duration_ms: lat, trace_id: trace, msg }),
            attributes: { service: s.name, route, status: String(status), trace_id: trace },
          });
          recordTrace(trace, s.name, route, now + offs[i] - lat, lat, status);
        }
      }
    });
  }
}

// ---- traces: one per logged request, fanning out along the call graph ----
const CALLS = { checkout: ["auth", "inventory", "payments"], search: ["catalog"], catalog: ["inventory"], payments: ["postgres"], inventory: ["postgres"], auth: ["postgres"] };
const traceStore = new Map(); // traceId -> { spans }
const MAX_TRACES = 30000; // ~20 min at the mock's rate, so trace links from logs keep working
const spanId = () => hex() + hex();

function recordTrace(traceId, service, route, start, durationMs, status) {
  const spans = [];
  const add = (svc, name, kind, st, dur, parent, err) =>
    spans.push({ traceId, spanId: spanId(), parentSpanId: parent, service: svc, name, kind, start: st, durationMs: Math.round(dur * 100) / 100,
      status: err ? "error" : "ok", ...(err ? { statusMessage: ERR_MSGS[Math.floor(rnd() * ERR_MSGS.length)] } : {}),
      attributes: svc === "postgres" ? { "db.system": "postgresql", "db.statement": "SELECT … FROM orders WHERE id = $1" } : { "http.route": name.replace(/^\w+ /, ""), "http.status_code": String(err && kind === "server" ? status : 200) } });
  const isErr = status >= 500;
  add(service, route, "server", start, durationMs, undefined, isErr);
  const walk = (svc, parentId, st, budget, depth) => {
    const callees = CALLS[svc] ?? [];
    let t = st + budget * 0.05;
    callees.forEach((callee, k) => {
      const dur = (budget * 0.8) / callees.length * (0.6 + 0.4 * rnd());
      const failing = isErr && depth === 0 && k === callees.length - 1;
      add(svc, callee === "postgres" ? "SELECT orders" : `${callee} call`, "client", t, dur, parentId, failing);
      const client = spans[spans.length - 1].spanId;
      if (callee !== "postgres") {
        const inner = Math.max(0, dur - 0.6); // never negative for very short calls
        add(callee, `POST /${callee}`, "server", t + 0.3, inner, client, failing);
        walk(callee, spans[spans.length - 1].spanId, t + 0.3, inner, depth + 1);
      }
      t += dur;
    });
  };
  if (depth0Calls(service)) walk(service, spans[0].spanId, start, durationMs, 0);
  traceStore.set(traceId, { spans });
  if (traceStore.size > MAX_TRACES) traceStore.delete(traceStore.keys().next().value);
}
const depth0Calls = (svc) => (CALLS[svc] ?? []).length > 0;

function summarize(id, t) {
  const root = t.spans.find((s) => !s.parentSpanId) ?? t.spans[0];
  const end = Math.max(...t.spans.map((s) => s.start + s.durationMs));
  return { traceId: id, rootService: root.service, rootName: root.name, start: root.start, durationMs: Math.round((end - root.start) * 100) / 100,
    spans: t.spans.length, errors: t.spans.filter((s) => s.status === "error").length, services: [...new Set(t.spans.map((s) => s.service))].sort() };
}

function searchTraces(p) {
  const limit = Math.min(500, Number(p.get("limit")) || 50);
  const svc = p.get("service"), name = (p.get("name") ?? "").toLowerCase(), minDur = Number(p.get("minDurationMs")) || 0;
  const errOnly = p.get("error") === "true", start = Number(p.get("start")) || 0, end = Number(p.get("end")) || Infinity;
  const out = [];
  for (const [id, t] of [...traceStore.entries()].reverse()) {
    const s = summarize(id, t);
    if (svc && !s.services.includes(svc)) continue;
    if (name && !t.spans.some((x) => x.name.toLowerCase().includes(name))) continue;
    if (s.durationMs < minDur || (errOnly && !s.errors) || s.start < start || s.start > end) continue;
    out.push(s);
    if (out.length >= limit) break;
  }
  return out;
}

// Backfill 30 minutes at 10 s, like generator.backfill.
const boot = Date.now();
for (let t = boot - 30 * 60_000; t < boot; t += 10_000) tick(t, 10_000, false);
let nextIncident = boot + 60_000;
setInterval(() => {
  const now = Date.now();
  if (now > nextIncident) {
    const s = services[Math.floor(rnd() * services.length)];
    s.incidentTo = now + 90_000;
    nextIncident = now + 4 * 60_000;
    decide(`synthetic incident started on ${s.name}`);
  }
  tick(now, 1000, true);
  const cut = now - KEEP_MS;
  for (const s of series.values()) { let i = 0; while (i < s.pts.length && s.pts[i].t < cut) i++; if (i) s.pts.splice(0, i); }
  while (logs.length && (logs[0].timestamp < cut || logs.length > 200_000)) logs.shift();
}, 1000);

// ---- query engine (subset of internal/query) ----
function matchDims(have, want = {}) {
  const hk = Object.keys(have), wk = Object.keys(want);
  if (hk.length !== wk.length) return false;
  return wk.every((k) => k in have && (want[k] === "*" || have[k] === want[k]));
}

function reduce(stat, vals) {
  if (!vals.length) return null;
  switch (stat) {
    case "Sum": return vals.reduce((a, b) => a + b, 0);
    case "SampleCount": return vals.length;
    case "Minimum": return Math.min(...vals);
    case "Maximum": return Math.max(...vals);
    case "Average": case "": return vals.reduce((a, b) => a + b, 0) / vals.length;
  }
  const m = /^p(\d+(?:\.\d+)?)$/.exec(stat);
  if (!m) throw Object.assign(new Error(`bad request: unknown stat "${stat}"`), { status: 400 });
  const s = [...vals].sort((a, b) => a - b);
  const rank = Math.min(s.length - 1, Math.max(0, Math.ceil((Number(m[1]) / 100) * s.length) - 1));
  return s[rank];
}

function fetchStat(ms, start, end, period) {
  const p = period * 1000;
  const buckets = new Map();
  for (const s of series.values()) {
    if (s.meta.namespace !== ms.namespace || s.meta.metricName !== ms.metricName || !matchDims(s.meta.dimensions, ms.dimensions)) continue;
    for (const pt of s.pts) {
      if (pt.t < start || pt.t >= end) continue;
      const b = Math.floor(pt.t / p) * p;
      (buckets.get(b) ?? buckets.set(b, []).get(b)).push(pt.v);
    }
  }
  const ts = [...buckets.keys()].sort((a, b) => a - b);
  return { timestamps: ts, values: ts.map((t) => reduce(ms.stat, buckets.get(t))) };
}

// Metric math, a subset: + - * / and parentheses over query ids and numbers,
// evaluated per aligned timestamp. The real engine has functions too (RATE,
// FILL, ...); the mock reports those as unsupported.
function evalExpr(expr, results) {
  const toks = expr.match(/\s*([A-Za-z_][A-Za-z0-9_]*|\d+(?:\.\d+)?|[-+*/()])/g)?.map((s) => s.trim()) ?? [];
  if (toks.join("") !== expr.replace(/\s+/g, "")) throw new Error(`mock: cannot parse "${expr}"`);
  const ts = [...new Set(toks.filter((t) => results.has(t)).flatMap((t) => results.get(t).timestamps))].sort((a, b) => a - b);
  let i = 0;
  const peek = () => toks[i];
  const at = (t) => {
    i = 0;
    const v = sum(t);
    if (i !== toks.length) throw new Error(`mock: unexpected "${toks[i]}"`);
    return v;
  };
  function atom(t) {
    const tok = toks[i++];
    if (tok === "(") { const v = sum(t); i++; return v; }
    if (tok === "-") return -atom(t);
    if (/^\d/.test(tok)) return Number(tok);
    const r = results.get(tok);
    if (!r) throw new Error(`mock: unknown id or unsupported function "${tok}"`);
    const k = r.timestamps.indexOf(t);
    return k < 0 ? NaN : r.values[k];
  }
  function prod(t) { let v = atom(t); while (peek() === "*" || peek() === "/") { const op = toks[i++]; const w = atom(t); v = op === "*" ? v * w : v / w; } return v; }
  function sum(t) { let v = prod(t); while (peek() === "+" || peek() === "-") { const op = toks[i++]; const w = prod(t); v = op === "+" ? v + w : v - w; } return v; }
  const out = { timestamps: [], values: [] };
  for (const t of ts) {
    const v = at(t);
    if (Number.isFinite(v)) { out.timestamps.push(t); out.values.push(v); }
  }
  return out;
}

function runQuery(req) {
  if (!Array.isArray(req.queries) || !req.queries.length) throw Object.assign(new Error("bad request: at least one query is required"), { status: 400 });
  if (!(req.period > 0)) throw Object.assign(new Error("bad request: period must be positive"), { status: 400 });
  const results = new Map();
  for (const q of req.queries) if (q.metricStat) results.set(q.id, fetchStat(q.metricStat, req.start, req.end, req.period));
  for (const q of req.queries) {
    if (q.metricStat) continue;
    try {
      results.set(q.id, { ...evalExpr(q.expression ?? "", results) });
    } catch (e) {
      results.set(q.id, { timestamps: [], values: [], error: e.message });
    }
  }
  return req.queries
    .filter((q) => q.returnData !== false)
    .map((q) => {
      const r = results.get(q.id);
      if (r.error) return { id: q.id, label: q.label ?? q.id, timestamps: [], values: [], statusCode: "InternalError", messages: [r.error] };
      return { id: q.id, label: q.label ?? q.id, timestamps: r.timestamps, values: r.values, statusCode: "Complete" };
    });
}

// ---- anomaly (a seasonal mean ± 2σ stand-in for Holt-Winters; same shape) ----
function anomaly(values, season) {
  return values.map((actual, i) => {
    if (i < season) return { forecast: null, lower: null, upper: null, actual, anomalous: false };
    const hist = values.slice(Math.max(0, i - 3 * season), i);
    const mean = hist.reduce((a, b) => a + b, 0) / hist.length;
    const sd = Math.sqrt(hist.reduce((a, b) => a + (b - mean) ** 2, 0) / hist.length) || 1e-9;
    const lower = mean - 2 * sd, upper = mean + 2 * sd;
    return { forecast: mean, lower, upper, actual, anomalous: actual < lower || actual > upper };
  });
}

// ---- alarm simulator (M of N state machine, like alarms.Simulate) ----
function simulate(cfg, values) {
  const n = Math.max(1, cfg.evaluationPeriods | 0), m = Math.max(1, Math.min(n, (cfg.datapointsToAlarm | 0) || n));
  const breach = (v) => ({ GreaterThanThreshold: v > cfg.threshold, GreaterThanOrEqualToThreshold: v >= cfg.threshold, LessThanThreshold: v < cfg.threshold, LessThanOrEqualToThreshold: v <= cfg.threshold })[cfg.comparison] ?? false;
  const win = [];
  let state = "INSUFFICIENT_DATA", alarmCount = 0, breachCount = 0;
  const states = values.map((v) => {
    let b;
    if (v == null) {
      b = cfg.treatMissingData === "breaching" ? true : cfg.treatMissingData === "notBreaching" ? false : null;
      if (cfg.treatMissingData === "ignore") return state;
    } else {
      b = breach(v);
      if (b) breachCount++;
    }
    win.push(b);
    if (win.length > n) win.shift();
    const known = win.filter((x) => x != null);
    let next = state;
    if (win.filter(Boolean).length >= m) next = "ALARM";
    else if (known.length === 0) next = "INSUFFICIENT_DATA";
    else next = "OK";
    if (next === "ALARM" && state !== "ALARM") alarmCount++;
    state = next;
    return state;
  });
  return { states, alarmCount, breachCount };
}

// ---- forecast (damped Holt with a small grid, like internal/forecast's short-history method) ----
function holt(y, h, interval = 0.8) {
  const z = { 0.5: 0.6744898, 0.8: 1.2815516, 0.9: 1.6448536, 0.95: 1.959964 }[interval] ?? 1.2815516;
  let best = null;
  for (const phi of [0.9, 0.98, 1])
    for (const a of [0.1, 0.3, 0.5, 0.7, 0.9])
      for (const b of [0.01, 0.05, 0.2]) {
        let l = y[0], tr = y[1] - y[0], sse = 0;
        for (const v of y.slice(1)) {
          const f = l + phi * tr;
          sse += (v - f) ** 2;
          const nl = a * v + (1 - a) * (l + phi * tr);
          tr = b * (nl - l) + (1 - b) * phi * tr;
          l = nl;
        }
        if (!best || sse < best.sse * (1 - 1e-9)) best = { l, tr, a, b, phi, sse };
      }
  const sigma = Math.sqrt(best.sse / (y.length - 1));
  const nonNeg = y.every((v) => v >= 0);
  const c = (v) => (nonNeg ? Math.max(0, v) : v);
  const out = [];
  let damp = 0, varSum = 0;
  for (let k = 1; k <= h; k++) {
    damp += best.phi ** k;
    if (k > 1) varSum += (best.a * (1 + (k - 1) * best.b)) ** 2;
    const f = best.l + damp * best.tr, w = z * sigma * Math.sqrt(1 + varSum);
    out.push({ forecast: c(f), lower: c(f - w), upper: c(f + w) });
  }
  return out;
}
const breaches = (cmp, v, thr) => ({ GreaterThanThreshold: v > thr, GreaterThanOrEqualToThreshold: v >= thr, LessThanThreshold: v < thr, LessThanOrEqualToThreshold: v <= thr })[cmp];

function forecast(req) {
  const bad = (m) => Object.assign(new Error(`bad request: ${m}`), { status: 400 });
  if (!(req.period >= 1 && req.period <= 86400)) throw bad("period must be in [1,86400]");
  if (!(req.horizon >= 1 && req.horizon <= 5000)) throw bad("horizon must be in [1,5000]");
  const p = req.period * 1000;
  const end = Math.floor((req.end ?? Date.now()) / p) * p;
  let hist;
  if (req.values?.length) hist = req.values.map((v, i) => ({ t: end - (req.values.length - i) * p, v }));
  else {
    if (!req.metricStat && !req.expression) throw bad("metricStat, expression or values is required");
    const start = Math.floor((req.start ?? end - 2 * Math.floor(86400 / req.period) * p) / p) * p;
    const qs = [];
    if (req.metricStat) qs.push({ id: "m1", metricStat: req.metricStat, returnData: !req.expression });
    if (req.expression) qs.push({ id: "e1", expression: req.expression });
    const r = runQuery({ queries: qs, start, end, period: req.period }).at(-1);
    const at = new Map(r.timestamps.map((t, i) => [t, r.values[i]]));
    hist = [];
    for (let t = start; t < end; t += p) hist.push({ t, v: at.has(t) ? at.get(t) : null });
  }
  const first = hist.findIndex((x) => x.v != null);
  const y = first < 0 ? [] : hist.slice(first).map((x) => x.v);
  for (let i = 1; i < y.length; i++) if (y[i] == null) y[i] = y[i - 1];
  if (y.length < 3) throw bad("forecast: need at least 3 values");
  const season = req.season || Math.floor(86400 / req.period);
  const method = req.method ?? "auto";
  let fallback;
  if (method === "model") fallback = "no model service configured (--forecaster-url)";
  else if ((method === "holtwinters" || method === "stl") && y.length < 2 * season)
    fallback = `${method} needs two seasons (${2 * season} points), have ${y.length}; used holt`;
  const last = hist.length ? hist.at(-1).t : end - p;
  const points = holt(y, req.horizon, req.interval || 0.8).map((x, i) => ({ t: last + (i + 1) * p, ...x }));
  const res = { method: "holt", history: hist, points, crossesAt: null };
  if (fallback) res.fallback = fallback;
  if (req.threshold != null) res.crossesAt = points.find((x) => breaches(req.comparison, x.forecast, req.threshold))?.t ?? null;
  return res;
}

// ---- predictive alarm replay (internal/api/backtest.go, over the mock's short history) ----
function backtest(rule, hours = 24) {
  const bad = (m) => Object.assign(new Error(`bad request: ${m}`), { status: 400 });
  if (!rule?.metric?.namespace || !rule.metric.metricName) throw bad("metric is required");
  if (!(rule.forecastHorizonSec > 0)) throw bad("rule.forecastHorizonSec must be set: backtest replays predictive rules");
  const period = rule.periodSec || 60, p = period * 1000;
  const end = Math.floor(Date.now() / p) * p, start = end - hours * 3600_000;
  const n = Math.round((end - start) / p);
  const { timestamps, values } = fetchStat(rule.metric, start, end, period);
  const at = new Map(timestamps.map((t, i) => [t, values[i]]));
  const obs = Array.from({ length: n }, (_, i) => at.get(start + i * p) ?? null);
  const stride = Math.max(1, Math.ceil(n / 96));
  const horizon = Math.ceil(rule.forecastHorizonSec / period);
  const less = /^Less/.test(rule.config.comparison);
  const forecastVals = new Array(n).fill(null);
  for (let i = 0; i < n; i += stride) {
    const hist = obs.slice(Math.max(0, i - 119), i + 1).filter((v) => v != null);
    if (hist.length < 3) continue;
    const f = holt(hist, horizon).map((x) => x.forecast);
    const worst = less ? Math.min(...f) : Math.max(...f);
    for (let k = i; k < Math.min(n, i + stride); k++) forecastVals[k] = worst;
  }
  const predicted = simulate(rule.config, forecastVals), observed = simulate(rule.config, obs);
  const first = (s) => s.indexOf("ALARM");
  const fp = first(predicted.states), fo = first(observed.states);
  return { forecast: forecastVals, predicted, observed, leadSec: fp >= 0 && fo >= 0 ? (fo - fp) * period : null, stride, start, method: "built-in auto" };
}

// ---- reports (shape of internal/reports.Report, over the mock's short history) ----
const REPORT_RANGES = { "24h": 86400e3, "7d": 7 * 86400e3, "30d": 30 * 86400e3 };
function report(rangeId) {
  const dur = REPORT_RANGES[rangeId];
  if (!dur) throw Object.assign(new Error("bad request: range must be 24h, 7d or 30d"), { status: 400 });
  const end = Math.floor(Date.now() / 60_000) * 60_000, start = end - dur;
  const q = (metricName, stat, svc) => fetchStat({ namespace: NS, metricName, stat, dimensions: { Service: svc, Route: "*" } }, start, end, 60);
  const sumv = (r) => r.values.reduce((a, b) => a + b, 0);
  const wmean = (p, w) => {
    const wt = new Map(w.timestamps.map((t, i) => [t, w.values[i]]));
    let n = 0, d = 0;
    p.timestamps.forEach((t, i) => { const x = wt.get(t) ?? 0; n += p.values[i] * x; d += x; });
    return d ? n / d : null;
  };
  const row = (svc) => {
    const req = q("Requests", "Sum", svc), err = q("Errors", "Sum", svc), p99 = q("Latency", "p99", svc), p50 = q("Latency", "p50", svc);
    const errAt = new Map(err.timestamps.map((t, i) => [t, err.values[i]]));
    const live = req.values.filter((v) => v > 0).length;
    const good = req.timestamps.filter((t, i) => req.values[i] > 0 && (errAt.get(t) ?? 0) / req.values[i] < 0.05).length;
    const r = sumv(req);
    // The mock keeps 35 minutes: the "previous period" is the first half of that.
    const mid = req.timestamps[Math.floor(req.timestamps.length / 2)] ?? end;
    const part = (a, b) => ({ timestamps: p99.timestamps.filter((t) => t >= a && t < b), values: p99.values.filter((_, i) => p99.timestamps[i] >= a && p99.timestamps[i] < b) });
    const now99 = wmean(part(mid, end), req), old99 = wmean(part(start, mid), req);
    return {
      name: svc, requests: r, errorPct: r ? (sumv(err) / r) * 100 : null, p99: wmean(p99, req), p50: wmean(p50, req),
      p99ChangePct: now99 != null && old99 ? ((now99 - old99) / old99) * 100 : null, availabilityPct: live ? (good / live) * 100 : null,
    };
  };
  const tot = row("*");
  const svcs = services.map((s) => row(s.name)).sort((a, b) => b.requests - a.requests).map(({ p50: _p50, ...r }) => r);
  const alarmRows = rules.map((r) => {
    const trs = history.filter((t) => t.ruleId === r.id).slice().reverse();
    let fired = 0, ms = 0, since = start, inAlarm = false;
    for (const t of trs) {
      const ts = Date.parse(t.timestamp);
      if (inAlarm) ms += ts - since;
      if (t.to === "ALARM") fired++;
      inAlarm = t.to === "ALARM";
      since = ts;
    }
    if (inAlarm) ms += Date.now() - since;
    return { id: r.id, name: r.name, fired, minutesInAlarm: Math.round(ms / 6000) / 10, state: r.state };
  }).sort((a, b) => b.fired - a.fired || b.minutesInAlarm - a.minutesInAlarm);
  const anomalies = [];
  for (const s of services) {
    const p99 = q("Latency", "p99", s.name);
    anomaly(p99.values, 10).forEach((b, i) => {
      if (b.anomalous) anomalies.push({ service: s.name, metric: "Latency p99", at: p99.timestamps[i], value: b.actual, expectedLower: b.lower, expectedUpper: b.upper });
    });
  }
  anomalies.sort((a, b) => b.at - a.at);
  const slowTraces = [...traceStore.keys()].map((id) => summarize(id, traceStore.get(id))).filter((t) => t.start >= start).sort((a, b) => b.durationMs - a.durationMs).slice(0, 10);
  const capacity = rules.filter((r) => r.enabled && r.config.threshold).slice(0, 20).map((r) => {
    let f;
    try {
      f = forecast({ metricStat: r.metric, period: r.periodSec, horizon: Math.min(5000, dur / 1000 / r.periodSec), start: end - 30 * 60_000, end, threshold: r.config.threshold, comparison: r.config.comparison });
    } catch {
      return null;
    }
    const svc = r.metric.dimensions?.Service;
    return { label: r.name, ...(svc && svc !== "*" ? { service: svc } : {}), metric: `${r.metric.metricName} ${r.metric.stat}`, threshold: r.config.threshold, crossesAt: f.crossesAt, method: f.method };
  }).filter(Boolean).sort((a, b) => (a.crossesAt ?? Infinity) - (b.crossesAt ?? Infinity));
  const firing = alarmRows.filter((a) => a.state === "ALARM").map((a) => a.name);
  const fired = alarmRows.reduce((a, b) => a + b.fired, 0);
  let narrative = `Served ${Math.round(tot.requests).toLocaleString("en-US")} requests at a ${(tot.errorPct ?? 0).toFixed(2)}% error rate, p99 ${Math.round(tot.p99 ?? 0)} ms.`;
  if (fired) narrative += ` Alarms fired ${fired === 1 ? "once" : `${fired} times`}.`;
  if (firing.length) narrative += ` In alarm now: ${firing.join(", ")}.`;
  if (anomalies.length) narrative += ` ${Math.min(20, anomalies.length)} anomalies detected.`;
  return {
    generatedAt: new Date().toISOString(), range: { id: rangeId, start, end },
    totals: { requests: tot.requests, errorPct: tot.errorPct, p50: tot.p50, p99: tot.p99, availabilityPct: tot.availabilityPct },
    services: svcs, alarms: alarmRows, anomalies: anomalies.slice(0, 20), slowTraces, capacity, narrative,
  };
}

// ---- dashboards (versioned, compare-and-swap like metadata.SQLStore) ----
const boards = new Map();
let boardSeq = 0;
const now0 = () => new Date().toISOString();
function validBoard(d) {
  if (!d.title || !String(d.title).trim()) return "title is required (max 200 chars)";
  const allowed = ["timeseries", "stat", "logs", "alarm", "text", "anomaly", "forecast"];
  for (const [i, w] of (d.widgets ?? []).entries()) {
    if (!allowed.includes(w.type)) return `widget ${i}: unknown type "${w.type}"`;
    if (!(w.w > 0 && w.h > 0 && w.x >= 0 && w.y >= 0 && w.x + w.w <= 12)) return `widget ${i}: invalid grid position`;
  }
  return null;
}
function saveBoard(d, id) {
  const bad = validBoard(d);
  if (bad) throw Object.assign(new Error(bad), { status: 400 });
  const widgets = (d.widgets ?? []).map((w) => ({ ...w, id: w.id || `w${++boardSeq}` }));
  if (!id) {
    const b = { ...d, id: `d${++boardSeq}`, tenantId: 1, widgets, version: 1, updatedAt: now0() };
    boards.set(b.id, b);
    return b;
  }
  const cur = boards.get(id);
  if (!cur) throw Object.assign(new Error("resource not found"), { status: 404 });
  if (cur.version !== d.version) throw Object.assign(new Error("version conflict; reload and retry"), { status: 409 });
  const b = { ...d, id, tenantId: 1, widgets, version: cur.version + 1, updatedAt: now0() };
  boards.set(id, b);
  return b;
}
// The node board RedBrain synthesizes from ZeroWatch/Host (internal/supervisor/host.go).
{
  const hq = (id, label, metricName, stat, extra = {}) => ({ id, label, metricStat: { namespace: HOST_NS, metricName, stat, dimensions: { Host: HOST, ...extra } } });
  const fw = (title, q, period, horizonMin, threshold, x, y) => ({ type: "forecast", title, query: { queries: [q], period, horizonMin, threshold }, x, y, w: 6, h: 4 });
  saveBoard({
    title: `Auto · Host ${HOST}`, description: `CPU, memory, disks and network of ${HOST}, with forecasts`,
    layout: { autogen: true, schema: 2, range: "1h", refresh: 30 },
    widgets: [
      { type: "stat", title: "CPU used %", query: { queries: [hq("c", "cpu", "CPUUtilization", "Average")], period: 60 }, x: 0, y: 0, w: 3, h: 2 },
      { type: "stat", title: "Memory used %", query: { queries: [hq("m", "memory", "MemoryUsedPercent", "Average")], period: 60 }, x: 3, y: 0, w: 3, h: 2 },
      { type: "stat", title: "Fullest disk %", query: { queries: [hq("d", "disk", "DiskUsedPercent", "Maximum", { Mount: "*" })], period: 60 }, x: 6, y: 0, w: 3, h: 2 },
      { type: "stat", title: "Load average (1 min)", query: { queries: [hq("l", "load", "LoadAverage1", "Average")], period: 60 }, x: 9, y: 0, w: 3, h: 2 },
      fw("Memory used % · next 6 h", hq("m", "memory %", "MemoryUsedPercent", "Average"), 60, 360, 95, 0, 2),
      fw("CPU used % · next 6 h", hq("c", "cpu %", "CPUUtilization", "Average"), 60, 360, 90, 6, 2),
      fw("Disk / used % · next 24 h", hq("d", "/", "DiskUsedPercent", "Maximum", { Mount: "/" }), 300, 1440, 90, 0, 6),
      fw("Disk /data used % · next 24 h", hq("d", "/data", "DiskUsedPercent", "Maximum", { Mount: "/data" }), 300, 1440, 90, 6, 6),
      { type: "timeseries", title: "CPU %", query: { queries: [hq("u", "used", "CPUUtilization", "Average"), hq("w", "iowait", "CPUIOWait", "Average"), hq("s", "steal", "CPUSteal", "Average")], period: 60 }, x: 0, y: 10, w: 6, h: 4 },
      { type: "timeseries", title: "Memory and swap %", query: { queries: [hq("m", "memory %", "MemoryUsedPercent", "Average"), hq("s", "swap %", "SwapUsedPercent", "Average")], period: 60 }, x: 6, y: 10, w: 6, h: 4 },
      { type: "timeseries", title: "Disk I/O (bytes/s)", query: { queries: [hq("r0", "nvme0n1 read", "DiskReadBytes", "Average", { Device: "nvme0n1" }), hq("w0", "nvme0n1 write", "DiskWriteBytes", "Average", { Device: "nvme0n1" })], period: 60 }, x: 0, y: 14, w: 6, h: 4 },
      { type: "timeseries", title: "Network (bytes/s)", query: { queries: [hq("r0", "eth0 in", "NetworkRxBytes", "Average", { Interface: "eth0" }), hq("t0", "eth0 out", "NetworkTxBytes", "Average", { Interface: "eth0" })], period: 60 }, x: 6, y: 14, w: 6, h: 4 },
    ],
  });
}
const RED_ALL = { Route: "*", Service: "*" };
const mqs = (id, label, metricName, stat, dims) => ({ id, label, metricStat: { namespace: NS, metricName, stat, dimensions: dims } });
saveBoard({
  title: "Auto-RED · Services", description: "Synthesized by RedBrain from discovered RED series",
  layout: { autogen: true, schema: 2, range: "1h", refresh: 10 },
  widgets: [
    { type: "stat", title: "Total requests / min", query: { queries: [mqs("t", "requests", "Requests", "Sum", RED_ALL)], period: 60 }, x: 0, y: 0, w: 4, h: 2 },
    { type: "stat", title: "Total errors / min", query: { queries: [mqs("t", "errors", "Errors", "Sum", RED_ALL)], period: 60 }, x: 4, y: 0, w: 4, h: 2 },
    { type: "stat", title: "p99 latency (ms)", query: { queries: [mqs("t", "p99", "Latency", "p99", RED_ALL)], period: 60 }, x: 8, y: 0, w: 4, h: 2 },
    { type: "timeseries", title: "Rate · requests per service", query: { queries: services.map((s, i) => mqs(`r${i}`, s.name, "Requests", "Sum", { Route: "*", Service: s.name })), period: 60 }, x: 0, y: 2, w: 6, h: 4 },
    { type: "timeseries", title: "Errors per service", query: { queries: services.map((s, i) => mqs(`e${i}`, s.name, "Errors", "Sum", { Route: "*", Service: s.name })), period: 60 }, x: 6, y: 2, w: 6, h: 4 },
    { type: "timeseries", title: "Duration · p99 per service", query: { queries: services.map((s, i) => mqs(`l${i}`, s.name, "Latency", "p99", { Route: "*", Service: s.name })), period: 60 }, x: 0, y: 6, w: 12, h: 4 },
    { type: "logs", title: "Error logs", query: { level: "error" }, x: 0, y: 10, w: 12, h: 4 },
  ],
});
saveBoard({
  title: "Checkout deep-dive", description: "Hand-made example board",
  layout: { range: "1h", refresh: 30 },
  widgets: [
    { type: "text", title: "Notes", query: { text: "Checkout calls payments, inventory and auth. Error budget: 1 % 5xx." }, x: 0, y: 0, w: 4, h: 3 },
    { type: "anomaly", title: "Checkout p99 with expected band", query: { queries: [mqs("m1", "checkout p99", "Latency", "p99", { Route: "*", Service: "checkout" })], period: 60 }, x: 4, y: 0, w: 8, h: 3 },
    { type: "alarm", title: "Checkout alarms", query: { service: "checkout" }, x: 0, y: 3, w: 4, h: 2 },
    { type: "timeseries", title: "Error ratio %", query: { queries: [{ ...mqs("e", "errors", "Errors", "Sum", { Route: "*", Service: "checkout" }), returnData: false }, { ...mqs("r", "requests", "Requests", "Sum", { Route: "*", Service: "checkout" }), returnData: false }, { id: "ratio", label: "5xx %", expression: "100 * e / r" }], period: 60 }, x: 4, y: 3, w: 8, h: 3 },
  ],
});

// ---- logs ----
// storage.Terms: whitespace splits, double quotes group and are dropped.
function terms(s) {
  const out = [];
  let cur = "", quoted = false;
  for (const ch of s) {
    if (ch === '"') quoted = !quoted;
    else if (!quoted && /\s/.test(ch)) { if (cur) out.push(cur); cur = ""; }
    else cur += ch;
  }
  if (cur) out.push(cur);
  return out;
}

function matchLog(e, q) {
  if (q.logGroup && e.logGroup !== q.logGroup) return false;
  if (q.level && (e.level ?? "").toLowerCase() !== q.level.toLowerCase()) return false;
  if (q.start && e.timestamp < q.start) return false;
  if (q.end && e.timestamp > q.end) return false;
  const msg = e.message.toLowerCase();
  for (const raw of terms(q.query ?? "")) {
    const neg = raw.startsWith("-") && raw.length > 1;
    const term = (neg ? raw.slice(1) : raw).toLowerCase();
    const eq = term.indexOf("=");
    const hit = eq > 0 ? (e.attributes?.[term.slice(0, eq)] ?? "").toLowerCase() === term.slice(eq + 1) : msg.includes(term);
    if (hit === neg) return false;
  }
  return true;
}

function searchLogs(q) {
  const limit = Math.min(10_000, q.limit > 0 ? q.limit : 1000);
  const out = [];
  let scanned = 0;
  for (let i = logs.length - 1; i >= 0 && out.length < limit; i--) {
    scanned++;
    if (matchLog(logs[i], q)) out.push(logs[i]);
  }
  return { events: out, scanned, truncated: out.length === limit };
}

// ---- alarms (M of N over the rule's metric) ----
const rules = [
  { name: "checkout · p99 latency high", svc: "checkout", metricName: "Latency", stat: "p99", threshold: 0, auto: true },
  { name: "payments · p99 latency high", svc: "payments", metricName: "Latency", stat: "p99", threshold: 0, auto: true },
  { name: "search · p99 latency high", svc: "search", metricName: "Latency", stat: "p99", threshold: 0, auto: true },
  { name: "inventory · 5xx errors", svc: "inventory", metricName: "Errors", stat: "Sum", threshold: 25, auto: false },
].map((r, i) => ({
  id: `rule-${i + 1}`, tenantId: 1, name: r.name,
  description: r.auto ? "" : "More than 25 server errors per minute for 3 of 5 minutes.",
  metric: { namespace: NS, metricName: r.metricName, stat: r.stat, dimensions: { Service: r.svc, Route: "*" } },
  periodSec: 60, config: { threshold: r.threshold, comparison: "GreaterThanThreshold", evaluationPeriods: 5, datapointsToAlarm: 3, treatMissingData: "notBreaching" },
  actions: [{ type: "log" }], enabled: true, state: "INSUFFICIENT_DATA", updatedAt: new Date().toISOString(), _auto: r.auto,
}));
// Predictive capacity rules, as internal/supervisor/host.go synthesizes them.
for (const [name, metricName, stat, extra, periodSec, horizon, threshold, m, n] of [
  [`${HOST} · disk / will fill`, "DiskUsedPercent", "Maximum", { Mount: "/" }, 300, 86400, 90, 3, 3],
  [`${HOST} · disk /data will fill`, "DiskUsedPercent", "Maximum", { Mount: "/data" }, 300, 86400, 90, 3, 3],
  [`${HOST} · memory will run out`, "MemoryUsedPercent", "Average", {}, 60, 3600, 95, 3, 5],
])
  rules.push({
    id: `rule-${rules.length + 1}`, tenantId: 1, name, description: `Predictive: the ${horizon / 3600} h forecast crosses ${threshold} %.`,
    metric: { namespace: HOST_NS, metricName, stat, dimensions: { Host: HOST, ...extra } }, periodSec, forecastHorizonSec: horizon,
    config: { threshold, comparison: "GreaterThanThreshold", evaluationPeriods: n, datapointsToAlarm: m, treatMissingData: "missing" },
    actions: [{ type: "log" }], enabled: true, state: "INSUFFICIENT_DATA", updatedAt: new Date().toISOString(), _auto: false,
  });
const history = [];
const savedQueries = [{ id: "q1", tenantId: 1, name: "checkout errors", query: "level=error service=checkout", createdAt: new Date().toISOString() }];

function calibrate() {
  const now = Date.now();
  for (const r of rules) {
    if (!r._auto || r.config.threshold) continue;
    const { values } = fetchStat(r.metric, now - 30 * 60_000, now, 60);
    if (values.length < 5) continue;
    const mean = values.reduce((a, b) => a + b, 0) / values.length;
    r.config.threshold = Math.max(50, Math.floor(2 * mean));
    r.description = `Auto-calibrated: 2x 30-min baseline p99 (${mean.toFixed(1)} ms)`;
    decide(`synthesized alarm "${r.name}" threshold ${r.config.threshold}ms`);
  }
}

function evaluateAlarms() {
  const now = Date.now();
  const p = 60_000;
  const end = Math.floor(now / p) * p;
  for (const r of rules) {
    if (!r.config.threshold) continue;
    let { timestamps, values } = fetchStat(r.metric, end - 5 * p, end, 60);
    if (r.forecastHorizonSec) {
      // Predictive: compare the worst forecast value in the horizon (simplified to one step per evaluation).
      try {
        const f = forecast({ metricStat: r.metric, period: r.periodSec, horizon: Math.ceil(r.forecastHorizonSec / r.periodSec), start: end - 30 * p, end });
        values = Array(r.config.datapointsToAlarm).fill(Math.max(...f.points.map((x) => x.forecast)));
      } catch {
        timestamps = [];
      }
    }
    const breaching = values.filter((v) => v > r.config.threshold).length;
    const next = timestamps.length === 0 ? "INSUFFICIENT_DATA" : breaching >= r.config.datapointsToAlarm ? "ALARM" : "OK";
    if (next !== r.state) {
      const tr = { ruleId: r.id, ruleName: r.name, tenantId: 1, from: r.state, to: next, reason: `${breaching} of 5 periods over ${r.config.threshold}`, value: values.at(-1), timestamp: new Date().toISOString() };
      history.unshift(tr);
      r.state = next;
      r.updatedAt = tr.timestamp;
      decide(`alarm "${r.name}" moved ${tr.from} -> ${next} (${tr.reason})`);
      const frame = `event: alarm\ndata: ${JSON.stringify({ kind: "alarm", time: now, data: tr })}\n\n`;
      for (const res of sse) res.write(frame);
    }
  }
}
calibrate();
evaluateAlarms();
setInterval(() => { calibrate(); evaluateAlarms(); }, 10_000);

// ---- supervisor ----
function decide(msg) {
  decisions.push(`${new Date().toLocaleTimeString("en-GB", { hour12: false })} ${msg}`);
  if (decisions.length > 50) decisions.shift();
}
decide("probe procfs now watches 4 port(s)");
decide('synthesized dashboard "Auto-RED · Services"');
decide('synthesized dashboard "Auto · Host network"');
decide('synthesized dashboard "Auto · nginx"');
let cycle = 1;
setInterval(() => cycle++, 10_000);
const supervisor = () => ({
  cycle, lastRun: new Date(Date.now() - (Date.now() % 10_000)).toISOString(),
  host: { hostname: "zw-mock", os: "linux", arch: "amd64", kernel: "6.8.0", cpus: 8, btf: true, ebpfCapable: false, inContainer: false, inKubernetes: false, cloud: "", systemd: true },
  services: [
    { pid: 812, ppid: 1, comm: "nginx", cmdline: "nginx: master process", kind: "nginx", rssKb: 9_800, threads: 1, ports: [80] },
    { pid: 1204, ppid: 1, comm: "postgres", cmdline: "postgres -D /var/lib/postgresql", kind: "postgres", rssKb: 61_000, threads: 1, ports: [5432] },
    { pid: 2210, ppid: 1, comm: "node", cmdline: "node server.js", kind: "", rssKb: 88_400, threads: 11, ports: [3000] },
    { pid: 4411, ppid: 1, comm: "python3", cmdline: "python3 -m notifier", kind: "", rssKb: 41_200, threads: 4, ports: [8088] },
  ],
  containers: [],
  targets: [{ pid: 812, port: 80, service: "nginx" }, { pid: 1204, port: 5432, service: "postgres" }, { pid: 2210, port: 3000, service: "node" }, { pid: 4411, port: 8088, service: "python3" }],
  probe: { name: "procprobe", mode: "procfs", healthy: true, targets: 4, events: cycle * 37, emitted: cycle * 12, lastEmit: new Date().toISOString() },
  logSources: [
    { path: "/var/log/nginx/access.log", size: 1_840_221, logGroup: "/host/nginx/access", guess: { format: "nginx", confidence: 0.97, sampled: 20 }, offset: 1_840_221, shipped: 3 * cycle + 120, lastSeen: new Date().toISOString() },
    { path: "/var/log/postgresql/postgresql-16-main.log", size: 220_011, logGroup: "/host/postgresql", guess: { format: "syslog", confidence: 0.71, sampled: 20 }, offset: 220_011, shipped: 41, lastSeen: new Date().toISOString() },
    { path: "/var/log/app/notifier.jsonl", size: 90_442, logGroup: "/host/app/notifier", guess: { format: "json", confidence: 1, sampled: 20 }, offset: 90_442, shipped: 12 + cycle, lastSeen: new Date().toISOString() },
  ],
  synthesized: ["dashboard: Auto-RED · Services", "dashboard: Auto · Host network", "dashboard: Auto · nginx", ...rules.filter((r) => r._auto && r.config.threshold).map((r) => `alarm: ${r.name}`)],
  watchdog: { supervisorCpuPct: 0.38, processCpuPct: 2.1, rssMb: 64.2, sampleRate: 1, throttled: false },
  decisions: [...decisions],
  topology: topology(),
});

// Who talks to whom. Local service node ids equal the RED "Service" names so
// the UI can join metrics onto nodes; external peers are "ext:<ip>".
function topology() {
  const wobble = (n) => Math.max(1, Math.round(n * (0.8 + 0.4 * rnd())));
  const svc = (id, port) => ({ id, label: id, kind: "service", pid: 3000 + id.length * 7, ports: [port] });
  const ext = (ip) => ({ id: `ext:${ip}`, label: ip, kind: "external" });
  const nodes = [
    ext("203.0.113.24"), ext("198.51.100.7"), ext("other"),
    svc("checkout", 8080), svc("search", 8081), svc("catalog", 8082), svc("payments", 8083), svc("auth", 8084), svc("inventory", 8085),
    { id: "postgres", label: "postgres", kind: "service", pid: 1204, ports: [5432] },
  ];
  nodes[2].label = "other external peers";
  const e = (source, target, port, n) => ({ source, target, port, connections: wobble(n) });
  return {
    nodes,
    edges: [
      e("ext:203.0.113.24", "checkout", 8080, 14), e("ext:198.51.100.7", "search", 8081, 9), e("ext:other", "checkout", 8080, 22), e("ext:other", "search", 8081, 6),
      e("checkout", "payments", 8083, 6), e("checkout", "inventory", 8085, 4), e("checkout", "auth", 8084, 8), e("search", "catalog", 8082, 5),
      e("catalog", "inventory", 8085, 3), e("payments", "postgres", 5432, 4), e("inventory", "postgres", 5432, 3), e("auth", "postgres", 5432, 2),
    ],
  };
}

// ---- HTTP ----
const json = (res, status, body) => {
  res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  res.end(JSON.stringify(body));
};
const readBody = (req) => new Promise((ok, bad) => {
  let s = "";
  req.on("data", (c) => (s += c));
  req.on("end", () => { try { ok(s ? JSON.parse(s) : {}); } catch (e) { bad(Object.assign(new Error("bad request: invalid JSON"), { status: 400 })); } });
});
const strip = ({ _auto, ...r }) => r;
const TYPES = { ".html": "text/html; charset=utf-8", ".js": "text/javascript", ".css": "text/css", ".woff2": "font/woff2", ".svg": "image/svg+xml", ".json": "application/json" };

async function serveStatic(res, path) {
  const safe = normalize(path).replace(/^([/\\])+/, "");
  let file = join(DIST, safe);
  try {
    if (!safe || !(await stat(file)).isFile()) file = join(DIST, "index.html");
  } catch {
    file = join(DIST, "index.html");
  }
  try {
    const body = await readFile(file);
    const cache = file.includes(`${join("dist", "assets")}`) ? "public, max-age=31536000, immutable" : "no-cache";
    res.writeHead(200, { "Content-Type": TYPES[extname(file)] ?? "application/octet-stream", "Cache-Control": cache });
    res.end(body);
  } catch {
    res.writeHead(404, { "Content-Type": "text/plain" });
    res.end("ui not built: run npm run build");
  }
}

// ---- auth (shape of internal/authn). Off by default, like a dev node; with
// MOCK_AUTH=oidc, /auth/login signs in at once (no provider) and the API
// answers 401 without the session cookie.
const MOCK_OIDC = process.env.MOCK_AUTH === "oidc";
const MOCK_ROLE = process.env.MOCK_ROLE ?? "editor";
const signedIn = (req) => process.env.MOCK_SIGNED_IN === "1" || /(?:^|;\s*)zw_session=mock(?:;|$)/.test(req.headers.cookie ?? "");
function authRoute(req, res, url, route) {
  if (route === "GET /api/v1/auth/me") {
    if (!MOCK_OIDC) return json(res, 200, { authenticated: true, user: "", role: "admin", tenant: 1, method: "none", oidc: false });
    return json(res, 200, signedIn(req)
      ? { authenticated: true, user: "ada@example.com", sub: "mock-user", role: MOCK_ROLE, tenant: 1, method: "oidc", oidc: true }
      : { authenticated: false, user: "", role: "", tenant: 0, method: "none", oidc: true });
  }
  if (!MOCK_OIDC) return false;
  const back = (cookie, to) => {
    res.writeHead(302, { Location: to, "Set-Cookie": cookie, "Cache-Control": "no-store" });
    res.end();
  };
  if (route === "GET /auth/login") {
    const ret = url.searchParams.get("return") ?? "/";
    return back("zw_session=mock; Path=/; HttpOnly; SameSite=Lax", ret.startsWith("/") && !ret.startsWith("//") ? ret : "/");
  }
  if (route === "GET /auth/logout" || route === "POST /auth/logout") return back("zw_session=; Path=/; Max-Age=0", "/");
  if (url.pathname.startsWith("/api/") && !signedIn(req))
    return json(res, 401, { error: { code: "UNAUTHORIZED", message: "sign in required (/auth/login) or an API key" } });
  return false;
}

createServer(async (req, res) => {
  const url = new URL(req.url ?? "/", "http://localhost");
  const p = url.pathname;
  const route = `${req.method} ${p}`;
  try {
    if (route === "GET /healthz") return json(res, 200, { status: "ok" });
    if (authRoute(req, res, url, route) !== false) return;
    if (route === "GET /api/v1/stats")
      return json(res, 200, {
        storage: { backend: "memory", series: series.size, samples: [...series.values()].reduce((a, s) => a + s.pts.length, 0), chunkBytes: 0, bytesPerPoint: 1.37, rollupBuckets: 0, logEvents: logs.length },
        ingest: { metricsAccepted, metricsRejected: 0, logsAccepted, logsRejected: 0, emfExtracted: 0 },
        live: { subscribers: sse.size, published, dropped: 0 },
        role: "all", node: "zw-mock", ebpfBuild: false,
        commitLog: { published: metricsAccepted + logsAccepted, delivered: metricsAccepted + logsAccepted, dropped: 0, depth: 0, capacity: 65536 },
      });
    if (route === "GET /api/v1/series") {
      const ns = url.searchParams.get("namespace"), mn = url.searchParams.get("metricName");
      const limit = Number(url.searchParams.get("limit") ?? 1000);
      return json(res, 200, { series: [...series.values()].map((s) => s.meta).filter((m) => (!ns || m.namespace === ns) && (!mn || m.metricName === mn)).slice(0, limit) });
    }
    if (route === "POST /api/v1/query") return json(res, 200, { results: runQuery(await readBody(req)) });
    if (route === "GET /api/v1/logs/groups") {
      const g = new Map();
      for (const e of logs) {
        const x = g.get(e.logGroup) ?? { name: e.logGroup, streams: new Set(), events: 0, lastEvent: 0 };
        x.streams.add(e.logStream); x.events++; x.lastEvent = Math.max(x.lastEvent, e.timestamp);
        g.set(e.logGroup, x);
      }
      return json(res, 200, { logGroups: [...g.values()].map((x) => ({ ...x, streams: [...x.streams] })).sort((a, b) => a.name.localeCompare(b.name)) });
    }
    if (route === "POST /api/v1/logs/search") return json(res, 200, searchLogs(await readBody(req)));
    if (route === "GET /api/v1/logs/saved") return json(res, 200, { queries: savedQueries });
    if (route === "POST /api/v1/logs/saved") {
      const b = await readBody(req);
      const name = String(b.name ?? "").trim();
      if (!name || name.length > 200) throw Object.assign(new Error("name is required (max 200 chars)"), { status: 400 });
      const q = { id: `q${Date.now() % 1e9}`, tenantId: 1, name, query: String(b.query ?? "").trim(), ...(b.logGroup ? { logGroup: b.logGroup } : {}), createdAt: now0() };
      savedQueries.unshift(q);
      return json(res, 200, q);
    }
    const sq = /^\/api\/v1\/logs\/saved\/([^/]+)$/.exec(p);
    if (sq && req.method === "DELETE") {
      const i = savedQueries.findIndex((q) => q.id === sq[1]);
      if (i < 0) throw Object.assign(new Error("resource not found"), { status: 404 });
      savedQueries.splice(i, 1);
      return json(res, 200, { deleted: true });
    }
    if (route === "GET /api/v1/logs/tail") {
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive", "X-Accel-Buffering": "no" });
      res.write(": connected\n\n");
      sse.add(res);
      const ping = setInterval(() => res.write(": ping\n\n"), 15_000);
      req.on("close", () => { clearInterval(ping); sse.delete(res); });
      return;
    }
    if (route === "GET /api/v1/alarms") return json(res, 200, { alarms: rules.map(strip) });
    if (route === "POST /api/v1/alarms/backtest") {
      const b = await readBody(req);
      return json(res, 200, backtest(b.rule, b.hours ?? 24));
    }
    if (route === "POST /api/v1/alarms/simulate") {
      const b = await readBody(req);
      if (!Array.isArray(b.values) || b.values.length > 10_000) throw Object.assign(new Error("max 10000 values"), { status: 400 });
      return json(res, 200, simulate(b.config ?? {}, b.values));
    }
    if (route === "POST /api/v1/anomaly") {
      const b = await readBody(req);
      return json(res, 200, { bands: anomaly(b.values ?? [], b.season >= 2 ? b.season : 60) });
    }
    const am = /^\/api\/v1\/alarms\/([^/]+)$/.exec(p);
    if (route === "POST /api/v1/alarms" || (am && req.method === "PUT")) {
      const b = await readBody(req);
      if (!b.name || !b.metric?.namespace || !b.metric?.metricName) throw Object.assign(new Error("name and metric are required"), { status: 400 });
      const id = am ? am[1] : `rule-${rules.length + 1}-${Date.now() % 10_000}`;
      const i = rules.findIndex((r) => r.id === id);
      if (am && i < 0) throw Object.assign(new Error("resource not found"), { status: 404 });
      const r = { ...(i >= 0 ? rules[i] : { state: "INSUFFICIENT_DATA" }), ...b, id, tenantId: 1, enabled: b.enabled ?? true, updatedAt: now0(), _auto: false };
      if (i >= 0) rules[i] = r;
      else rules.push(r);
      decide(`${i >= 0 ? "updated" : "created"} alarm "${r.name}"`);
      return json(res, 200, strip(r));
    }
    if (am && req.method === "DELETE") {
      const i = rules.findIndex((r) => r.id === am[1]);
      if (i < 0) throw Object.assign(new Error("resource not found"), { status: 404 });
      rules.splice(i, 1);
      return json(res, 200, { deleted: true });
    }
    if (route === "GET /api/v1/dashboards")
      return json(res, 200, { dashboards: [...boards.values()].map((b) => ({ id: b.id, title: b.title, widgets: b.widgets.length, updatedAt: b.updatedAt })) });
    if (route === "POST /api/v1/dashboards") return json(res, 200, saveBoard(await readBody(req)));
    const dm = /^\/api\/v1\/dashboards\/([^/]+)$/.exec(p);
    if (dm && req.method === "GET") {
      const b = boards.get(dm[1]);
      return b ? json(res, 200, b) : json(res, 404, { error: { code: "NOT_FOUND", message: "resource not found" } });
    }
    if (dm && req.method === "PUT") return json(res, 200, saveBoard(await readBody(req), dm[1]));
    if (dm && req.method === "DELETE") {
      if (!boards.delete(dm[1])) throw Object.assign(new Error("resource not found"), { status: 404 });
      return json(res, 200, { deleted: true });
    }
    const h = /^\/api\/v1\/alarms\/([^/]+)\/history$/.exec(p);
    if (req.method === "GET" && h) {
      const limit = Number(url.searchParams.get("limit") ?? 100);
      return json(res, 200, { history: history.filter((t) => t.ruleId === h[1]).slice(0, limit) });
    }
    if (route === "GET /api/v1/supervisor") return json(res, 200, supervisor());
    if (route === "POST /api/v1/forecast") return json(res, 200, forecast(await readBody(req)));
    if (route === "GET /api/v1/reports/summary") return json(res, 200, report(url.searchParams.get("range") ?? "24h"));
    if (route === "POST /api/v1/reports/send") throw Object.assign(new Error("no report targets configured (--report-targets)"), { status: 400 });
    if (route === "GET /api/v1/traces") return json(res, 200, { traces: searchTraces(url.searchParams) });
    const tm = /^\/api\/v1\/traces\/([0-9a-f]+)$/.exec(p);
    if (tm && req.method === "GET") {
      const t = traceStore.get(tm[1]);
      return t ? json(res, 200, { traceId: tm[1], spans: t.spans }) : json(res, 404, { error: { code: "NOT_FOUND", message: "trace not found" } });
    }
    if (p.startsWith("/api/")) return json(res, 404, { error: { code: "NOT_FOUND", message: "mock: not simulated" } });
    if (req.method === "GET") return serveStatic(res, p);
    json(res, 405, { error: { code: "METHOD_NOT_ALLOWED", message: "method not allowed" } });
  } catch (e) {
    json(res, e.status ?? 500, { error: { code: e.status === 400 ? "BAD_REQUEST" : "INTERNAL", message: e.message } });
  }
}).listen(PORT, () => console.log(`zerowatch mock API on http://localhost:${PORT} (serving ${DIST})`));
