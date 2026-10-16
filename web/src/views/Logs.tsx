/**
 * Log Explorer: a volume histogram over the last 15 minutes and a live tail,
 * or, given a time window (#/logs?from=..&to=..), the lines of that window
 * with five minutes of context either side. Attribute facets beside the tail
 * toggle key=value terms in the filter. The filter uses the server's query
 * syntax: plain terms must all match, key=value matches attributes, -term excludes.
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { endpoints, type LogEvent } from "@/lib/api";
import { matchLog, subscribeLive } from "@/lib/live";
import { useDebounced, usePoll } from "@/lib/hooks";
import { clock, num, plural } from "@/lib/format";
import { logsLink } from "@/lib/range";
import { Empty, ErrorNote, PageHead, Section, StatusMark, stagger } from "@/design/primitives";
import { describeLog, levelTone } from "@/lib/status";
import { control, toneFill, toneText, typography, type Tone } from "@/design/tokens";

const HIST_MIN = 15;
const BUCKETS = 60;
const MAX_LINES = 500;
/** Context drawn either side of a selected window in the histogram. */
const PAD_MS = 5 * 60_000;
const LEVELS = ["", "error", "warn", "info"] as const;
/** Keys with more distinct values than this in the sample show a count instead of values. */
const HIGH_CARDINALITY = 50;
const ALWAYS_HIGH = new Set(["trace_id", "span_id", "request_id"]);
const FACET_KEYS = 8;
const FACET_VALUES = 5;

const isError = (e: LogEvent) => levelTone(e.level) === "critical";

/** `#/logs` with only the filter: live mode. */
function liveHash(q: string): string {
  return "#/logs" + (q ? "?q=" + encodeURIComponent(q) : "");
}

export function Logs({ initialQuery, from, to }: { initialQuery: string; from?: number; to?: number }) {
  const [query, setQuery] = useState(initialQuery);
  const [group, setGroup] = useState("");
  const [level, setLevel] = useState<(typeof LEVELS)[number]>("");
  const [paused, setPaused] = useState(false);
  const [hover, setHover] = useState(false);
  const q = useDebounced(query, 300);
  // A fixed window [from, to] replaces the live tail.
  const win = from != null && to != null && to > from ? { from, to } : null;

  useEffect(() => setQuery(initialQuery), [initialQuery]);

  const groups = usePoll(() => endpoints.logGroups(), 30_000);
  const hist = usePoll(
    async (signal) => {
      const end = win ? win.to + PAD_MS : Date.now();
      const start = win ? win.from - PAD_MS : end - HIST_MIN * 60_000;
      const r = await endpoints.searchLogs(
        { start, end: win ? end : undefined, limit: 10_000, logGroup: group || undefined, query: q || undefined },
        signal,
      );
      return { ...r, start, end };
    },
    win ? 60_000 : 15_000,
    [group, q, win?.from, win?.to],
  );

  // The tail: newest first, seeded from search, then live (unless a window is set).
  const [lines, setLines] = useState<LogEvent[]>([]);
  const [seedError, setSeedError] = useState<Error>();
  const [seedTruncated, setSeedTruncated] = useState(false);
  const frozen = paused || hover || win != null;
  const frozenRef = useRef(frozen);
  frozenRef.current = frozen;
  const windowedRef = useRef(win != null);
  windowedRef.current = win != null;
  const filterRef = useRef({ q, group, level });
  filterRef.current = { q, group, level };

  useEffect(() => {
    const ctrl = new AbortController();
    endpoints
      .searchLogs(
        {
          limit: win ? MAX_LINES : 200,
          start: win?.from,
          end: win?.to,
          logGroup: group || undefined,
          level: level || undefined,
          query: q || undefined,
        },
        ctrl.signal,
      )
      .then((r) => {
        setLines(r.events.filter((e) => matchLog(e, q, group, level)).slice(0, MAX_LINES));
        setSeedTruncated(r.truncated);
        setSeedError(undefined);
      })
      .catch((e: unknown) => {
        if (!(e instanceof DOMException && e.name === "AbortError")) setSeedError(e instanceof Error ? e : new Error(String(e)));
      });
    return () => ctrl.abort();
    // win is rebuilt every render; its bounds are the real dependency.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q, group, level, win?.from, win?.to]);

  useEffect(() => {
    let pending: LogEvent[] = [];
    const flush = setInterval(() => {
      // A fixed window never takes live lines: they fall outside it.
      if (windowedRef.current) pending = [];
      if (!pending.length || frozenRef.current) return;
      const batch = pending.reverse();
      pending = [];
      setLines((old) => [...batch, ...old].slice(0, MAX_LINES));
    }, 250);
    const off = subscribeLive((ev) => {
      if (ev.kind !== "log" || windowedRef.current) return;
      const f = filterRef.current;
      if (matchLog(ev.log, f.q, f.group, f.level)) pending.push(ev.log);
      if (pending.length > MAX_LINES) pending = pending.slice(-MAX_LINES);
    });
    return () => {
      clearInterval(flush);
      off();
    };
  }, []);

  const bars = useMemo(() => {
    const end = hist.data?.end ?? Date.now();
    const start = hist.data?.start ?? end - HIST_MIN * 60_000;
    const step = (end - start) / BUCKETS;
    const all = new Array<number>(BUCKETS).fill(0);
    const err = new Array<number>(BUCKETS).fill(0);
    for (const e of hist.data?.events ?? []) {
      const i = Math.floor((e.timestamp - start) / step);
      if (i < 0 || i >= BUCKETS) continue;
      all[i]++;
      if (isError(e)) err[i]++;
    }
    return { all, err, max: Math.max(1, ...all), start, end, step };
  }, [hist.data]);

  const facets = useMemo(() => buildFacets(hist.data?.events ?? []), [hist.data]);

  const groupCount = groups.data?.length ?? 0;
  const errorTotal = bars.err.reduce((a, b) => a + b, 0);
  const total = bars.all.reduce((a, b) => a + b, 0);
  const filter = query.trim();
  const terms = filter.split(/\s+/).filter(Boolean);
  const active = new Set(terms.map((t) => t.toLowerCase()));
  const span = bars.end - bars.start;
  const shadeLeft = win ? Math.max(0, ((win.from - bars.start) / span) * 100) : 0;
  const shadeWidth = win ? Math.min(100 - shadeLeft, ((win.to - Math.max(win.from, bars.start)) / span) * 100) : 0;

  const toggleTerm = (term: string) => {
    const t = term.toLowerCase();
    setQuery((active.has(t) ? terms.filter((x) => x.toLowerCase() !== t) : [...terms, term]).join(" "));
  };

  const facetPanel = <Facets facets={facets} active={active} onToggle={toggleTerm} sample={hist.data?.events.length ?? 0} truncated={hist.data?.truncated ?? false} />;

  return (
    <div className="space-y-12 sm:space-y-16">
      <PageHead
        eyebrow={win ? `Time window · ${plural(groupCount, "log group")} · live updates paused` : `Live tail · ${plural(groupCount, "log group")} · format inferred on arrival`}
        lines={[win ? "Logs, for a moment." : "Logs, as they land."]}
      />

      {win && (
        <div className="rise-in flex flex-wrap items-center justify-between gap-3 border border-border px-3 py-2.5" style={stagger(1)} role="status">
          <span className={`${typography.micro} tabular-nums`}>
            Showing {clock(win.from)}–{clock(win.to)}
            <span className="text-muted-foreground"> · live updates paused</span>
          </span>
          <button type="button" className={control.button} onClick={() => (window.location.hash = liveHash(filter))}>
            Back to live
          </button>
        </div>
      )}

      <Section
        index={1}
        title={win ? `Volume · ${clock(bars.start)}–${clock(bars.end)}` : `Volume · ${HIST_MIN} min`}
        note={
          <span className="inline-flex flex-wrap gap-x-4 gap-y-1">
            <span>{hist.data?.truncated ? `newest ${num(hist.data.events.length)} lines shown` : `${num(total)} lines · ${num(errorTotal)} errors`}</span>
            <span>select a bar for its lines</span>
          </span>
        }
      >
        {hist.data?.partial && (
          <p className="mb-3 text-[0.95rem]" role="status">
            <StatusMark tone="warning" label="Partial" /> <span className="ml-2">Some lines may be missing: {hist.data.partial}</span>
          </p>
        )}
        {hist.error && !hist.data ? (
          <ErrorNote error={hist.error} what="log volume" />
        ) : (
          <>
            <div
              className="relative flex h-16 items-end gap-[2px] border-b border-border"
              role="group"
              aria-label={`Log volume: ${num(total)} lines, ${num(errorTotal)} errors. Select a bar to show its lines.`}
            >
              {win && hist.data && shadeWidth > 0 && (
                <div
                  className="pointer-events-none absolute inset-y-0 border-x border-foreground/60 bg-foreground/10"
                  style={{ left: `${shadeLeft}%`, width: `${shadeWidth}%` }}
                  aria-hidden="true"
                />
              )}
              {bars.all.map((n, i) => {
                const t0 = bars.start + i * bars.step;
                const t1 = t0 + bars.step;
                const label = `${clock(t0)}–${clock(t1)} · ${n} lines · ${bars.err[i]} errors`;
                return (
                  <button
                    key={i}
                    type="button"
                    className={`bar-grow relative flex h-full min-w-0 flex-1 cursor-pointer flex-col justify-end hover:bg-muted focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring`}
                    style={stagger(i)}
                    title={label}
                    aria-label={`Show lines from ${label}`}
                    onClick={() => (window.location.hash = logsLink(t0, t1, filter))}
                  >
                    <span className="block bg-foreground/80" style={{ height: `${((n - bars.err[i]) / bars.max) * 100}%` }} />
                    <span className="block bg-tone-critical" style={{ height: `${(bars.err[i] / bars.max) * 100}%` }} />
                  </button>
                );
              })}
            </div>
            <div className={`${typography.eyebrow} mt-1.5 flex justify-between gap-3`} aria-hidden="true">
              <span className="tabular-nums">{win ? clock(bars.start) : `−${HIST_MIN} min`}</span>
              <span className="inline-flex flex-wrap items-center justify-center gap-x-4 gap-y-1">
                <span className="inline-flex items-center gap-2">
                  <span className="inline-block h-2.5 w-2.5 bg-tone-critical" />
                  error and fatal lines
                </span>
                {win && (
                  <span className="inline-flex items-center gap-2">
                    <span className="inline-block h-2.5 w-2.5 border border-foreground/60 bg-foreground/10" />
                    selected window
                  </span>
                )}
              </span>
              <span className="tabular-nums">{win ? clock(bars.end) : "now"}</span>
            </div>
          </>
        )}
      </Section>

      <Section
        index={2}
        title="Tail"
        note={
          win
            ? `${plural(lines.length, "line")} in window${seedTruncated ? ` · first ${MAX_LINES}` : ""} · newest first`
            : frozen
              ? hover && !paused
                ? "paused while the pointer is over it"
                : "paused"
              : "live · newest first"
        }
      >
        <div className="mb-4 flex flex-wrap items-center gap-2">
          <input
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="filter: timeout service=checkout -health"
            aria-label="Filter log lines"
            className={`${control.input} flex-[1_1_16rem]`}
          />
          <div className="relative min-w-[11rem] flex-[0_1_15rem]">
            <select value={group} onChange={(e) => setGroup(e.target.value)} className={control.select} aria-label="Log group">
              <option value="">All log groups</option>
              {(groups.data ?? []).map((g) => (
                <option key={g.name} value={g.name}>
                  {g.name}
                </option>
              ))}
            </select>
            <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[0.6rem]" aria-hidden="true">
              ▼
            </span>
          </div>
          <div className="flex flex-wrap gap-2" role="group" aria-label="Level">
            {LEVELS.map((l) => (
              <button key={l || "all"} type="button" className={level === l ? control.buttonActive : control.button} aria-pressed={level === l} onClick={() => setLevel(l)}>
                {l || "All"}
              </button>
            ))}
          </div>
          {!win && (
            <button type="button" className={paused ? control.buttonActive : control.button} aria-pressed={paused} onClick={() => setPaused((p) => !p)}>
              {paused ? "Resume" : "Pause"}
            </button>
          )}
        </div>
        <SavedSearches
          query={query}
          group={group}
          onApply={(q, g) => {
            setQuery(q);
            setGroup(g ?? "");
          }}
        />

        <details className="mb-4 border border-border lg:hidden">
          <summary className={`${typography.micro} cursor-pointer px-3 py-2.5`}>Attributes · {plural(facets.length, "key")}</summary>
          <div className="border-t border-hair px-3 pb-3 pt-3">{facetPanel}</div>
        </details>

        <div className="grid gap-6 lg:grid-cols-[16rem_minmax(0,1fr)]">
          <aside className="hidden min-w-0 lg:block" aria-label="Attributes">
            <p className={`${typography.eyebrow} mb-3`}>Attributes · select a value to filter</p>
            {facetPanel}
          </aside>
          <div className="min-w-0">
            {seedError && lines.length === 0 ? (
              <ErrorNote error={seedError} what="log lines" />
            ) : lines.length === 0 ? (
              <Empty>
                {win ? "No lines match in this window. Clear the filter, or go back to live." : "No lines match. Clear the filter, or wait: new lines appear here as they arrive."}
              </Empty>
            ) : (
              <div
                className="relative h-[32rem] overflow-auto border border-border font-mono text-[0.78rem]"
                role="log"
                aria-live="off"
                onMouseEnter={() => setHover(true)}
                onMouseLeave={() => setHover(false)}
              >
                {lines.map((e, i) => (
                  <LogLine key={`${e.timestamp}-${e.logStream}-${i}`} e={e} fresh={i < 20} />
                ))}
              </div>
            )}
          </div>
        </div>
      </Section>
    </div>
  );
}

interface Facet {
  key: string;
  /** lines in the sample carrying this key */
  count: number;
  distinct: number;
  /** top values; empty when the key has too many distinct values to list */
  values: { value: string; count: number }[];
}

/** The most common attribute keys in a sample, with their most common values. */
function buildFacets(events: LogEvent[]): Facet[] {
  const keys = new Map<string, Map<string, number>>();
  for (const e of events) {
    for (const [k, v] of Object.entries(e.attributes ?? {})) {
      let m = keys.get(k);
      if (!m) keys.set(k, (m = new Map()));
      m.set(v, (m.get(v) ?? 0) + 1);
    }
  }
  return [...keys.entries()]
    .map(([key, m]) => {
      let count = 0;
      for (const n of m.values()) count += n;
      const high = ALWAYS_HIGH.has(key) || m.size > HIGH_CARDINALITY;
      const values = high
        ? []
        : [...m.entries()]
            .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
            .slice(0, FACET_VALUES)
            .map(([value, n]) => ({ value, count: n }));
      return { key, count, distinct: m.size, values };
    })
    .sort((a, b) => Number(b.values.length > 0) - Number(a.values.length > 0) || b.count - a.count || a.key.localeCompare(b.key))
    .slice(0, FACET_KEYS);
}

/** HTTP status values get a status square; every other attribute stays uncoloured. */
function statusTone(key: string, value: string): Tone | null {
  if (key.toLowerCase() !== "status") return null;
  if (/^5\d\d$/.test(value)) return "critical";
  if (/^4\d\d$/.test(value)) return "warning";
  return null;
}

function Facets({ facets, active, onToggle, sample, truncated }: { facets: Facet[]; active: Set<string>; onToggle: (term: string) => void; sample: number; truncated: boolean }) {
  if (facets.length === 0) {
    return <p className="text-[0.9rem] text-muted-foreground">No attributes on the lines in view.</p>;
  }
  return (
    <div className="space-y-5">
      {facets.map((f, fi) => (
        <div key={f.key} className="min-w-0">
          <div className={`${typography.micro} flex justify-between gap-3 border-b border-hair pb-1.5`}>
            <span className="truncate" title={f.key}>
              {f.key}
            </span>
            <span className="shrink-0 tabular-nums text-muted-foreground">{f.values.length ? num(f.count) : `${num(f.distinct)} distinct`}</span>
          </div>
          {f.values.map((v, vi) => {
            const term = `${f.key}=${v.value}`;
            const on = active.has(term.toLowerCase());
            // The filter splits on whitespace, so a value containing a space cannot be matched as key=value.
            const filterable = v.value !== "" && !/\s/.test(term);
            const tone = statusTone(f.key, v.value);
            const body = (
              <>
                <span className="flex items-baseline justify-between gap-3">
                  <span className="inline-flex min-w-0 items-center gap-2">
                    {tone && <span className={`inline-block h-2.5 w-2.5 shrink-0 ${toneFill[tone]}`} aria-hidden="true" />}
                    <span className="truncate">{v.value || "(empty)"}</span>
                    {tone && <span className={`shrink-0 ${on ? "" : "text-muted-foreground"}`}>{tone === "critical" ? "5xx" : "4xx"}</span>}
                  </span>
                  <span className="shrink-0 tabular-nums">{num(v.count)}</span>
                </span>
                <span className={`mt-1 block h-px w-full ${on ? "bg-background/30" : "bg-hair"}`}>
                  <span className={`grow-x block h-px ${on ? "bg-background" : "bg-foreground/70"}`} style={stagger(fi + vi, { width: `${(v.count / Math.max(1, f.count)) * 100}%` })} />
                </span>
              </>
            );
            return filterable ? (
              <button
                key={v.value}
                type="button"
                className={`block w-full px-1.5 py-1.5 text-left font-mono text-[0.74rem] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring ${on ? "bg-foreground text-background" : "hover:bg-muted"}`}
                aria-pressed={on}
                title={on ? `Remove ${term} from the filter` : `Filter by ${term}`}
                onClick={() => onToggle(term)}
              >
                {body}
              </button>
            ) : (
              <div key={v.value} className="px-1.5 py-1.5 font-mono text-[0.74rem] text-muted-foreground" title="Contains a space, which the filter syntax cannot match">
                {body}
              </div>
            );
          })}
        </div>
      ))}
      <p className={typography.eyebrow}>
        from {plural(sample, "line")} in the volume sample{truncated ? " (newest only)" : ""}
      </p>
    </div>
  );
}

function LogLine({ e, fresh }: { e: LogEvent; fresh: boolean }) {
  const tone = levelTone(e.level);
  const { text, extra } = useMemo(() => describeLog(e), [e]);
  const traceId = useMemo(() => traceIdOf(e), [e]);
  return (
    <div className={`grid grid-cols-[4.5rem_minmax(0,1fr)] gap-x-3 border-b border-hair px-3 py-1.5 hover:bg-muted sm:grid-cols-[7.5rem_4.5rem_minmax(0,12rem)_minmax(0,1fr)] ${fresh ? "row-in" : ""}`}>
      <span className="hidden text-muted-foreground tabular-nums sm:block">{clock(e.timestamp, true)}</span>
      <span className={`uppercase ${toneText[tone]}`}>{e.level || "—"}</span>
      <span className="hidden truncate text-muted-foreground sm:block" title={e.logGroup}>
        {e.logGroup.replace(/^\/zerowatch\/synthetic\//, "")}
      </span>
      <span className="[overflow-wrap:anywhere]">
        {text}
        {extra && <span className="text-muted-foreground"> · {extra}</span>}
        {traceId && (
          <>
            <span className="text-muted-foreground"> · </span>
            <a className="border-b border-foreground/40 pb-0.5 font-mono transition-colors hover:border-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring" href={`#/traces?id=${encodeURIComponent(traceId)}`} title="Open this trace">
              trace {traceId}
            </a>
          </>
        )}
      </span>
    </div>
  );
}

/** The trace id of a line: its trace_id attribute, or the trace_id field of a JSON line. */
function traceIdOf(e: LogEvent): string {
  const a = e.attributes?.trace_id;
  if (a) return a;
  if (!e.message.startsWith("{")) return "";
  try {
    const v = (JSON.parse(e.message) as Record<string, unknown>).trace_id;
    return typeof v === "string" ? v : "";
  } catch {
    return "";
  }
}

/** A JSON line shows its message, with the useful fields beside it; other lines show as written. */

/** Saved searches (filter text + log group) as chips: click to apply, × to delete, or save the current one. */
function SavedSearches({ query, group, onApply }: { query: string; group: string; onApply: (query: string, group?: string) => void }) {
  const saved = usePoll(() => endpoints.savedQueries(), 60_000);
  const [naming, setNaming] = useState(false);
  const [name, setName] = useState("");
  const [error, setError] = useState<string>();
  const canSave = query.trim() !== "" || group !== "";
  const save = async () => {
    if (!name.trim()) return;
    try {
      await endpoints.saveQuery({ name: name.trim(), query: query.trim(), ...(group ? { logGroup: group } : {}) });
      setNaming(false);
      setName("");
      setError(undefined);
      saved.refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  const remove = async (id: string) => {
    try {
      await endpoints.deleteSavedQuery(id);
      saved.refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  const list = saved.data ?? [];
  return (
    <div className="mb-4 flex flex-wrap items-center gap-2">
      <span className={typography.eyebrow}>Saved</span>
      {list.length === 0 && !naming && <span className="text-[0.9rem] text-muted-foreground">none yet</span>}
      {list.map((q) => (
        <span key={q.id} className="inline-flex border border-border">
          <button
            type="button"
            className={`${typography.micro} px-2.5 py-1.5 transition-colors hover:bg-foreground hover:text-background`}
            title={`${q.query || "(no filter)"}${q.logGroup ? ` · ${q.logGroup}` : ""}`}
            onClick={() => onApply(q.query, q.logGroup)}
          >
            {q.name}
          </button>
          <button
            type="button"
            className={`${typography.micro} border-l border-border px-2 py-1.5 text-muted-foreground transition-colors hover:bg-foreground hover:text-background`}
            aria-label={`Delete saved search ${q.name}`}
            onClick={() => void remove(q.id)}
          >
            ×
          </button>
        </span>
      ))}
      {naming ? (
        <form
          className="inline-flex flex-wrap gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
        >
          <input autoFocus className={`${control.input} w-44`} value={name} maxLength={200} placeholder="name this search" aria-label="Saved search name" onChange={(e) => setName(e.target.value)} />
          <button type="submit" className={control.buttonActive} disabled={!name.trim()}>
            Save
          </button>
          <button type="button" className={control.button} onClick={() => setNaming(false)}>
            Cancel
          </button>
        </form>
      ) : (
        <button type="button" className={control.button} disabled={!canSave} onClick={() => setNaming(true)} title={canSave ? undefined : "Type a filter or pick a log group first"}>
          + Save search
        </button>
      )}
      {error && <span className="text-[0.9rem] text-tone-critical">{error}</span>}
    </div>
  );
}
