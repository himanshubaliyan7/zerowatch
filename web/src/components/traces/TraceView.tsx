/**
 * One trace: what it was and how long it took, the waterfall of its spans,
 * the selected span's detail, and where the time went per service. Loaded
 * once; a trace still arriving can be reloaded by hand.
 */
import { useEffect, useMemo, useState } from "react";
import { ApiError, endpoints, type TraceDetail } from "@/lib/api";
import { clock, num, plural } from "@/lib/format";
import { Empty, ErrorNote, PageHead, Section, Skeleton, Stat, StatGrid, Verdict, stagger } from "@/design/primitives";
import { control, typography } from "@/design/tokens";
import { buildTrace, fmtMs, shortId } from "./model";
import { ServiceBreakdown, SpanDetail, traceLogsHref } from "./SpanDetail";
import { lastSearchHash } from "./TraceSearch";
import { Waterfall } from "./Waterfall";

export function TraceView({ id }: { id: string }) {
  const [data, setData] = useState<TraceDetail>();
  const [error, setError] = useState<Error>();
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    const ctrl = new AbortController();
    setError(undefined);
    endpoints
      .trace(id, ctrl.signal)
      .then((d) => setData(d))
      .catch((e: unknown) => {
        if (!(e instanceof DOMException && e.name === "AbortError")) setError(e instanceof Error ? e : new Error(String(e)));
      });
    return () => ctrl.abort();
  }, [id, nonce]);

  const model = useMemo(() => (data && data.traceId === id ? buildTrace(data.spans ?? []) : null), [data, id]);
  const [picked, setPicked] = useState<string | null>(null);
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  useEffect(() => {
    setPicked(null);
    setCollapsed(new Set());
  }, [id]);

  const back = (
    <a href={lastSearchHash()} className={control.link}>
      ← All traces
    </a>
  );

  if (error instanceof ApiError && error.status === 404) {
    return (
      <div className="space-y-12">
        <PageHead eyebrow={<>Trace · {shortId(id)}</>} lines={["No such trace."]} />
        <Empty hatch>
          No trace with id <span className="font-mono text-[0.8rem] [overflow-wrap:anywhere]">{id}</span> on this node. It may not have arrived yet, or it has
          aged out of the trace store. Its logs may still be there:{" "}
          <a className={control.link} href={traceLogsHref(id)}>
            logs for this trace →
          </a>
        </Empty>
        {back}
      </div>
    );
  }
  if (error && !model) {
    return (
      <div className="space-y-12">
        <PageHead eyebrow={<>Trace · {shortId(id)}</>} lines={["Trace."]} />
        <ErrorNote error={error} what="this trace" />
        {back}
      </div>
    );
  }
  if (!model) {
    return (
      <div className="space-y-6" aria-busy="true">
        <Skeleton className="h-4 w-56" />
        <Skeleton className="h-16 w-3/4" />
        <Skeleton className="h-28 w-full" />
        <Skeleton className="h-80 w-full" />
      </div>
    );
  }

  const firstError = model.rows.find((r) => r.span.status === "error");
  const selectedId = picked && model.rows.some((r) => r.span.spanId === picked) ? picked : (firstError ?? model.rows[0]).span.spanId;
  const selected = model.rows.find((r) => r.span.spanId === selectedId)!;
  const root = model.root;
  const toggle = (sid: string) =>
    setCollapsed((old) => {
      const n = new Set(old);
      if (n.has(sid)) n.delete(sid);
      else n.add(sid);
      return n;
    });

  return (
    <div className="space-y-12 sm:space-y-16">
      <PageHead
        eyebrow={
          <>
            Trace · <span className="normal-case">{shortId(id)}</span> · {clock(model.t0, true)}
          </>
        }
        lines={[`${root.service}.`, root.name]}
      />

      <div className="rise-in flex flex-wrap items-center gap-x-6 gap-y-3" style={stagger(2)}>
        {model.errors > 0 ? (
          <span className="inline-flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
            <Verdict tone="critical" label={`${plural(model.errors, "span")} failed`} />
            {firstError?.span.statusMessage && <span className="min-w-0 font-mono text-[0.8rem] [overflow-wrap:anywhere]">{firstError.span.statusMessage}</span>}
          </span>
        ) : (
          <Verdict tone="positive" label="All spans ok" />
        )}
        {model.orphans > 0 && <span className={typography.eyebrow}>{plural(model.orphans, "span")} with a parent not received</span>}
        <span className="ml-auto flex flex-wrap gap-x-6 gap-y-2">
          <a className={control.link} href={traceLogsHref(id)}>
            Logs for this trace →
          </a>
          <button type="button" className={control.link} onClick={() => setNonce((n) => n + 1)}>
            Reload
          </button>
          {back}
        </span>
      </div>

      <StatGrid>
        <Stat label="Duration" value={model.total} decimals={model.total < 10 ? 2 : model.total < 100 ? 1 : 0} unit="ms, first span start to last span end" />
        <Stat label="Spans" value={model.rows.length} unit={model.orphans ? `${num(model.orphans)} without their parent` : "in tree order below"} />
        <Stat label="Services" value={model.services.length} unit={model.services.join(" · ")} />
        <Stat label="Errors" value={model.errors} unit={model.errors ? "spans with status error" : "no span failed"} />
      </StatGrid>

      <Section
        index={3}
        title="Waterfall"
        note={`0 = first span start · ${fmtMs(model.total)} in all · select a span for detail`}
        action={
          model.rows.some((r) => r.children > 0) ? (
            <span className="flex gap-2">
              <button type="button" className={control.link} onClick={() => setCollapsed(new Set())}>
                Expand all
              </button>
              <button
                type="button"
                className={control.link}
                onClick={() => setCollapsed(new Set(model.rows.filter((r) => r.depth >= 1 && r.children > 0).map((r) => r.span.spanId)))}
              >
                Collapse
              </button>
            </span>
          ) : undefined
        }
      >
        <div className="grid items-start gap-8 xl:grid-cols-[minmax(0,1fr)_24rem]">
          <Waterfall model={model} selected={selectedId} onSelect={setPicked} collapsed={collapsed} onToggle={toggle} />
          <aside className="min-w-0" aria-label="Selected span">
            <p className={`${typography.micro} border-b border-border pb-2.5`}>
              Span · <span className="normal-case">{selected.span.name}</span>
            </p>
            <SpanDetail row={selected} model={model} />
          </aside>
        </div>
      </Section>

      <Section index={4} title="Time by service" note="self time: a span's duration minus the time its children cover">
        <ServiceBreakdown model={model} />
      </Section>
    </div>
  );
}
