/**
 * One shared Server-Sent Events connection to /api/v1/logs/tail for the whole
 * page (the hub caps subscribers server-side). Views subscribe to the kinds
 * they need; filtering is done here, client-side, with the same rules as the
 * server's log matcher.
 */
import { withKey, type AlarmTransition, type LogEvent } from "./api";

export type LiveEvent =
  | { kind: "log"; time: number; log: LogEvent }
  | { kind: "alarm"; time: number; data: AlarmTransition }
  | { kind: "supervisor"; time: number; data: Record<string, unknown> };

type Listener = (e: LiveEvent) => void;
type StatusListener = (connected: boolean) => void;

const listeners = new Set<Listener>();
const statusListeners = new Set<StatusListener>();
let source: EventSource | null = null;
let connected = false;

function setConnected(v: boolean) {
  if (v === connected) return;
  connected = v;
  statusListeners.forEach((fn) => fn(v));
}

function open() {
  if (source || typeof EventSource === "undefined") return;
  source = new EventSource(withKey("/api/v1/logs/tail"));
  source.onopen = () => setConnected(true);
  // EventSource reconnects by itself; we only report the gap.
  source.onerror = () => setConnected(false);
  for (const kind of ["log", "alarm", "supervisor"] as const) {
    source.addEventListener(kind, (msg) => {
      try {
        const ev = JSON.parse((msg as MessageEvent<string>).data) as LiveEvent;
        listeners.forEach((fn) => fn(ev));
      } catch {
        // A malformed frame is skipped, not fatal.
      }
    });
  }
}

/** Reopen after the API key changes. */
export function reconnectLive(): void {
  source?.close();
  source = null;
  setConnected(false);
  if (listeners.size) open();
}

export function subscribeLive(fn: Listener): () => void {
  listeners.add(fn);
  open();
  return () => {
    listeners.delete(fn);
  };
}

export function onLiveStatus(fn: StatusListener): () => void {
  statusListeners.add(fn);
  fn(connected);
  return () => {
    statusListeners.delete(fn);
  };
}

/**
 * Splits a filter into terms exactly like the server (storage.Terms):
 * whitespace separates terms, double quotes group text with spaces
 * (`route="GET /health"`, `"connection reset"`) and are dropped; an
 * unclosed quote runs to the end. Case is kept here; matching ignores it.
 */
export function filterTerms(query: string): string[] {
  const out: string[] = [];
  let cur = "";
  let quoted = false;
  for (const ch of query) {
    if (ch === '"') quoted = !quoted;
    else if (!quoted && /\s/.test(ch)) {
      if (cur) out.push(cur);
      cur = "";
    } else cur += ch;
  }
  if (cur) out.push(cur);
  return out;
}

/** Quotes a value for key=value when it contains whitespace. */
export function filterTerm(key: string, value: string): string {
  return /\s/.test(value) ? `${key}="${value}"` : `${key}=${value}`;
}

/**
 * Mirrors storage.matchLog: terms must all match (case-insensitive
 * substring of the message); key=value terms match attributes; a leading
 * "-" excludes. See filterTerms for quoting.
 */
export function matchLog(e: LogEvent, query: string, group: string, level: string): boolean {
  if (group && e.logGroup !== group) return false;
  if (level && (e.level ?? "").toLowerCase() !== level) return false;
  const msg = e.message.toLowerCase();
  for (const raw of filterTerms(query)) {
    const neg = raw.startsWith("-") && raw.length > 1;
    const term = (neg ? raw.slice(1) : raw).toLowerCase();
    let hit: boolean;
    const eq = term.indexOf("=");
    if (eq > 0) {
      const k = term.slice(0, eq);
      const v = term.slice(eq + 1);
      const attrs = e.attributes ?? {};
      const found = Object.entries(attrs).find(([ak]) => ak.toLowerCase() === k);
      // Like the server, level=… falls back to the event's level when no attribute has it.
      const value = found ? found[1] : k === "level" ? (e.level ?? "") : undefined;
      hit = value != null && value.toLowerCase() === v;
    } else {
      hit = msg.includes(term);
    }
    if (hit === neg) return false;
  }
  return true;
}

export interface RequestSample {
  t: number;
  latencyMs: number;
  status: number;
  service: string;
}

/**
 * Reads one request out of a log line: JSON with a status and a duration
 * (the synthetic generator, most structured access logs), or a plain
 * "... 200 31ms" line. Lines without both are not requests.
 */
export function parseRequest(e: LogEvent): RequestSample | null {
  let status = Number(e.attributes?.status ?? NaN);
  let latency = NaN;
  let service = e.attributes?.service ?? "";
  const m = e.message;
  if (m.startsWith("{")) {
    try {
      const j = JSON.parse(m) as Record<string, unknown>;
      const d = j.duration_ms ?? j.latency_ms ?? j.duration ?? j.elapsed_ms;
      if (typeof d === "number") latency = d;
      if (Number.isNaN(status) && typeof j.status === "number") status = j.status;
      if (!service && typeof j.service === "string") service = j.service;
    } catch {
      // Not JSON after all.
    }
  } else {
    const r = /\b([1-5]\d\d)\b\s+(\d+(?:\.\d+)?)\s?ms\b/.exec(m);
    if (r) {
      if (Number.isNaN(status)) status = Number(r[1]);
      latency = Number(r[2]);
    }
  }
  if (!Number.isFinite(status) || !Number.isFinite(latency) || status < 100 || status > 599) return null;
  return { t: e.timestamp, latencyMs: latency, status, service: service || e.logGroup };
}
