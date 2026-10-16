/**
 * Trace search: filters kept in the hash (replaced in place, so the view does
 * not remount), a duration scatter of every trace in the window, and the same
 * traces as ruled rows. Polls every 10 s.
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { endpoints, type TraceSummary } from "@/lib/api";
import { useDebounced, usePoll } from "@/lib/hooks";
import { listServices } from "@/lib/red";
import { clock, num, plural } from "@/lib/format";
import { RANGES, range } from "@/lib/range";
import { Empty, ErrorNote, PageHead, Section, Skeleton, StatusMark, stagger } from "@/design/primitives";
import { control, typography } from "@/design/tokens";
import { DurationScatter } from "./DurationScatter";
import { fmtMs } from "./model";

const LIMIT = 500;
const ROWS = 50;

interface Filters {
  service: string;
  name: string;
  min: string;
  errors: boolean;
  range: string;
}

/** The last search hash, so a trace's "all traces" link returns to the same filters. */
let lastSearch = "#/traces";
export const lastSearchHash = () => lastSearch;

export const traceHref = (id: string) => `#/traces?id=${encodeURIComponent(id)}`;

function readFilters(fallbackService?: string): Filters {
  const qs = window.location.hash.split("?")[1] ?? "";
  const p = new URLSearchParams(qs);
  const r = p.get("range") ?? "";
  return {
    service: p.get("service") ?? fallbackService ?? "",
    name: p.get("name") ?? "",
    min: p.get("min") ?? "",
    errors: p.get("errors") === "1",
    range: RANGES.some((x) => x.id === r) ? r : "1h",
  };
}

function hashFor(f: Filters): string {
  const p = new URLSearchParams();
  if (f.service) p.set("service", f.service);
  if (f.name) p.set("name", f.name);
  if (f.min) p.set("min", f.min);
  if (f.errors) p.set("errors", "1");
  if (f.range !== "1h") p.set("range", f.range);
  const qs = p.toString();
  return "#/traces" + (qs ? `?${qs}` : "");
}

export function TraceSearch({ service: initialService }: { service?: string }) {
  const [f, setF] = useState<Filters>(() => readFilters(initialService));
  const set = (patch: Partial<Filters>) => setF((old) => ({ ...old, ...patch }));
  const written = useRef("");

  // Filters live in the hash, replaced in place: no history entry, no remount.
  useEffect(() => {
    const h = hashFor(f);
    lastSearch = h;
    written.current = h;
    if (window.location.hash !== h) window.history.replaceState(null, "", h);
  }, [f]);

  // A link to #/traces (the nav, the map) while here resets to that link's filters.
  useEffect(() => {
    const on = () => {
      const h = window.location.hash;
      if (h.startsWith("#/traces") && !/[?&]id=/.test(h) && h !== written.current) setF(readFilters());
    };
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);

  const name = useDebounced(f.name.trim(), 300);
  const minRaw = useDebounced(f.min, 300);
  const minMs = Number(minRaw) > 0 ? Number(minRaw) : undefined;
  const r = range(f.range);

  const res = usePoll(
    async (signal) => {
      const end = Date.now();
      const start = end - r.ms;
      const traces = await endpoints.traces(
        { service: f.service || undefined, name: name || undefined, minDurationMs: minMs, error: f.errors || undefined, start, end, limit: LIMIT },
        signal,
      );
      return { traces, start, end };
    },
    10_000,
    [f.service, name, minMs, f.errors, r.id],
  );
  const known = usePoll(() => listServices(), 60_000);

  const services = useMemo(() => {
    const s = new Set(known.data ?? []);
    for (const t of res.data?.traces ?? []) for (const x of t.services) s.add(x);
    if (f.service) s.add(f.service);
    return [...s].sort();
  }, [known.data, res.data, f.service]);

  const traces = res.data?.traces ?? [];
  const errCount = traces.filter((t) => t.errors > 0).length;
  const filtered = !!(f.service || name || minMs || f.errors);
  const capped = traces.length >= LIMIT;
  const open = (id: string) => (window.location.hash = traceHref(id));

  const lines = !res.data
    ? ["Traces."]
    : traces.length === 0
      ? ["No traces."]
      : [`${capped ? "Newest " : ""}${plural(traces.length, "trace")}.`, errCount ? `${num(errCount)} with errors.` : "None with errors."];

  return (
    <div className="space-y-12 sm:space-y-16">
      <PageHead
        eyebrow={`Traces · last ${r.label}${f.service ? ` · through ${f.service}` : ""}${capped ? ` · newest ${num(LIMIT)} shown` : ""}`}
        lines={lines}
        lead={traces.length > 0 ? leadFor(traces) : undefined}
      />

      <Section index={1} title="Filter" note={res.data ? `updated ${clock(res.data.end)} · every 10 s` : undefined}>
        <div className="flex flex-wrap items-center gap-2">
          <div className="relative min-w-[10rem] flex-[0_1_14rem]">
            <select value={f.service} onChange={(e) => set({ service: e.target.value })} className={control.select} aria-label="Service">
              <option value="">All services</option>
              {services.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
            <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[0.6rem]" aria-hidden="true">
              ▼
            </span>
          </div>
          <input
            type="search"
            value={f.name}
            onChange={(e) => set({ name: e.target.value })}
            placeholder="operation contains: POST /api"
            aria-label="Operation name contains"
            className={`${control.input} flex-[1_1_14rem]`}
          />
          <label className="inline-flex flex-[0_1_10rem] items-center gap-2">
            <span className={typography.eyebrow}>min</span>
            <input
              type="number"
              inputMode="decimal"
              min={0}
              value={f.min}
              onChange={(e) => set({ min: e.target.value })}
              placeholder="ms"
              aria-label="Minimum duration in milliseconds"
              className={`${control.input} w-full`}
            />
          </label>
          <button type="button" className={f.errors ? control.buttonActive : control.button} aria-pressed={f.errors} onClick={() => set({ errors: !f.errors })}>
            Errors only
          </button>
          <div className="flex flex-wrap gap-2" role="group" aria-label="Time range">
            {RANGES.map((x) => (
              <button key={x.id} type="button" className={x.id === r.id ? control.buttonActive : control.button} aria-pressed={x.id === r.id} onClick={() => set({ range: x.id })}>
                {x.label}
              </button>
            ))}
          </div>
          {filtered && (
            <button type="button" className={control.link} onClick={() => setF((old) => ({ ...old, service: "", name: "", min: "", errors: false }))}>
              Clear filters
            </button>
          )}
        </div>
      </Section>

      {res.error && !res.data ? (
        <ErrorNote error={res.error} what="traces" />
      ) : !res.data ? (
        <div className="space-y-4" aria-busy="true">
          <Skeleton className="h-4 w-48" />
          <Skeleton className="h-[230px] w-full" />
          <Skeleton className="h-40 w-full" />
        </div>
      ) : traces.length === 0 ? (
        filtered ? (
          <Empty>No traces match these filters in the last {r.label}. Clear a filter or widen the range.</Empty>
        ) : (
          <Empty hatch>
            No traces in the last {r.label}. Traces arrive over OTLP: send spans to
            <span className="font-mono text-[0.8rem]"> POST /v1/traces</span> on this node, or run the synthetic generator
            (<span className="font-mono text-[0.8rem]">--generate=true</span>), which traces every request it makes up.
          </Empty>
        )
      ) : (
        <>
          <Section index={2} title="Duration over time" note={`${plural(traces.length, "trace")} · ${plural(errCount, "with errors", "with errors")}`}>
            {/* A capped result covers less than the range: the axis spans what came back. */}
            <DurationScatter traces={traces} start={capped ? Math.min(...traces.map((t) => t.start)) : res.data.start} end={res.data.end} onOpen={open} />
          </Section>
          <Section index={3} title="Traces" note="newest first · select a row to open">
            <TraceTable traces={traces} />
          </Section>
        </>
      )}
    </div>
  );
}

/** One sentence from the data: the typical and the slowest trace. */
function leadFor(traces: TraceSummary[]): string {
  const d = traces.map((t) => t.durationMs).sort((a, b) => a - b);
  const med = d[Math.floor((d.length - 1) / 2)];
  const slow = traces.reduce((a, t) => (t.durationMs > a.durationMs ? t : a), traces[0]);
  return `Median ${fmtMs(med)}. Slowest: ${slow.rootService} ${slow.rootName} at ${fmtMs(slow.durationMs)}, ${clock(slow.start)}.`;
}

const ROW_GRID = "grid grid-cols-[minmax(0,1fr)_auto] gap-x-4 gap-y-0.5 md:grid-cols-[minmax(0,1.6fr)_6rem_6.5rem_3.5rem_minmax(0,1fr)_7rem] md:items-baseline";

function TraceTable({ traces }: { traces: TraceSummary[] }) {
  const [all, setAll] = useState(false);
  const shown = all ? traces : traces.slice(0, ROWS);
  return (
    <div>
      <div className={`${ROW_GRID} ${typography.eyebrow} hidden border-b border-border pb-2 md:grid`} aria-hidden="true">
        <span>root service · operation</span>
        <span>start</span>
        <span className="text-right">duration</span>
        <span className="text-right">spans</span>
        <span>services</span>
        <span>errors</span>
      </div>
      <ul>
        {shown.map((t, i) => (
          <li key={t.traceId}>
            <a
              href={traceHref(t.traceId)}
              className={`${ROW_GRID} border-b border-hair py-2.5 transition-[background-color,padding] duration-200 hover:bg-muted hover:pl-3 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring ${i < 24 ? "rise-in" : ""}`}
              style={i < 24 ? stagger(i) : undefined}
            >
              <span className="min-w-0">
                <span className={`${typography.micro} mr-2 text-muted-foreground`}>{t.rootService}</span>
                <span className="text-[0.9rem] [overflow-wrap:anywhere]">{t.rootName}</span>
              </span>
              <span className="text-right font-mono text-[0.8rem] tabular-nums md:hidden">{fmtMs(t.durationMs)}</span>
              <span className={`${typography.micro} tabular-nums text-muted-foreground md:text-foreground`}>{clock(t.start)}</span>
              <span className="hidden text-right font-mono text-[0.8rem] tabular-nums md:block">{fmtMs(t.durationMs)}</span>
              <span className={`${typography.micro} hidden text-right tabular-nums md:block`}>{num(t.spans)}</span>
              <span className="col-span-2 min-w-0 font-mono text-[0.72rem] text-muted-foreground [overflow-wrap:anywhere] md:col-span-1">
                <span className="md:hidden">{plural(t.spans, "span")} · </span>
                {t.services.join(" ")}
              </span>
              <span className="col-span-2 md:col-span-1">
                {t.errors > 0 ? <StatusMark tone="critical" label={plural(t.errors, "error")} /> : <span className={`${typography.micro} text-muted-foreground`}>no errors</span>}
              </span>
            </a>
          </li>
        ))}
      </ul>
      {traces.length > ROWS && (
        <button type="button" className={`${control.button} mt-4`} onClick={() => setAll((a) => !a)}>
          {all ? `Show the newest ${ROWS}` : `Show all ${num(traces.length)}`}
        </button>
      )}
    </div>
  );
}
