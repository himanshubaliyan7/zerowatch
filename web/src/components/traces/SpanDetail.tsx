/**
 * The selected span, in ruled key/value rows, with links to the logs written
 * during it; and the trace's self time per service as small bars.
 */
import type { ReactNode } from "react";
import { clock, num, plural } from "@/lib/format";
import { logsLink } from "@/lib/range";
import { StatusMark, stagger } from "@/design/primitives";
import { control, typography } from "@/design/tokens";
import { fmtMs, type SpanRow, type TraceModel } from "./model";

/** Logs this far either side of a span. */
const AROUND_MS = 30_000;

export function traceLogsHref(traceId: string): string {
  return "#/logs?q=" + encodeURIComponent(`trace_id=${traceId}`);
}

function Row({ k, children }: { k: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-[7.5rem_minmax(0,1fr)] gap-x-3 border-t border-hair py-2">
      <dt className={typography.eyebrow}>{k}</dt>
      <dd className="min-w-0 text-[0.88rem] [overflow-wrap:anywhere]">{children}</dd>
    </div>
  );
}

export function SpanDetail({ row, model }: { row: SpanRow; model: TraceModel }) {
  const s = row.span;
  const attrs = Object.entries(s.attributes ?? {}).sort((a, b) => a[0].localeCompare(b[0]));
  return (
    <div className="min-w-0">
      <dl>
        <Row k="service">{s.service}</Row>
        <Row k="name">{s.name}</Row>
        <Row k="kind">{s.kind || "—"}</Row>
        <Row k="start">
          <span className="tabular-nums">
            +{fmtMs(s.start - model.t0)} <span className="text-muted-foreground">· {clock(s.start, true)}</span>
          </span>
        </Row>
        <Row k="duration">
          <span className="tabular-nums">
            {fmtMs(row.end - s.start)} <span className="text-muted-foreground">· self {fmtMs(row.self)}</span>
          </span>
        </Row>
        <Row k="status">
          {s.status === "error" ? (
            <StatusMark tone="critical" label="error" />
          ) : (
            <span className={`${typography.micro} ${s.status === "unset" ? "text-muted-foreground" : ""}`}>{s.status}</span>
          )}
          {s.statusMessage && <span className="mt-1 block font-mono text-[0.8rem]">{s.statusMessage}</span>}
        </Row>
        <Row k="span id">
          <span className="font-mono text-[0.8rem]">{s.spanId}</span>
        </Row>
        <Row k="parent">
          {row.orphan ? (
            <span>
              <span className="font-mono text-[0.8rem]">{s.parentSpanId}</span>
              <span className={`${typography.eyebrow} block`}>parent not received · shown at root level</span>
            </span>
          ) : s.parentSpanId ? (
            <span className="font-mono text-[0.8rem]">{s.parentSpanId}</span>
          ) : (
            <span className="text-muted-foreground">none · root span</span>
          )}
        </Row>
        {row.children > 0 && <Row k="below">{plural(row.descendants, "span")}</Row>}
      </dl>

      <p className={`${typography.eyebrow} mt-6 border-b border-border pb-2`}>Attributes · {num(attrs.length)}</p>
      {attrs.length === 0 ? (
        <p className="py-2 text-[0.88rem] text-muted-foreground">This span carries no attributes.</p>
      ) : (
        <dl className="font-mono text-[0.78rem]">
          {attrs.map(([k, v]) => (
            <div key={k} className="grid grid-cols-[minmax(0,10rem)_minmax(0,1fr)] gap-x-3 border-b border-hair py-1.5">
              <dt className="truncate text-muted-foreground" title={k}>
                {k}
              </dt>
              <dd className="min-w-0 [overflow-wrap:anywhere]">{v}</dd>
            </div>
          ))}
        </dl>
      )}

      <div className="mt-6 flex flex-col items-start gap-3">
        <a className={control.link} href={traceLogsHref(s.traceId)}>
          Logs for this trace →
        </a>
        <a className={control.link} href={logsLink(s.start - AROUND_MS, row.end + AROUND_MS, `service=${s.service}`)}>
          Logs for {s.service} around this span →
        </a>
      </div>
    </div>
  );
}

/** Where the trace's time went: each service's self time (time not spent waiting on a child span). */
export function ServiceBreakdown({ model }: { model: TraceModel }) {
  const all = model.selfByService.reduce((a, x) => a + x.ms, 0) || 1;
  const max = model.selfByService[0]?.ms || 1;
  return (
    <div>
      {model.selfByService.map((x, i) => (
        <div key={x.service} className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-1.5 border-t border-hair py-2.5 sm:grid-cols-[minmax(0,14rem)_minmax(0,1fr)_12rem]">
          <span className={`${typography.micro} truncate`} title={x.service}>
            {x.service}
          </span>
          <span className="order-last col-span-2 block h-2 bg-hair sm:order-none sm:col-span-1" aria-hidden="true">
            <span className="grow-x block h-2 bg-foreground" style={stagger(i, { width: `${(x.ms / max) * 100}%` })} />
          </span>
          <span className={`${typography.micro} text-right tabular-nums`}>
            {fmtMs(x.ms)} <span className="text-muted-foreground">· {num((x.ms / all) * 100)} % · {plural(x.spans, "span")}</span>
          </span>
        </div>
      ))}
    </div>
  );
}
