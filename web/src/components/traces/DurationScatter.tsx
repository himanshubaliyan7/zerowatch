/**
 * Every trace in the window as one mark: x = when it started, y = how long it
 * took, on a log scale so a 2 ms health check and a 2 s checkout share the
 * plot. Traces with errors are squares in the critical tone, the rest small
 * ink dots, so shape says it as well as colour. Selecting a mark opens it.
 */
import { useLayoutEffect, useMemo, useRef, useState, type MouseEvent } from "react";
import type { TraceSummary } from "@/lib/api";
import { clock, plural } from "@/lib/format";
import { chart, typography } from "@/design/tokens";
import { fmtMs } from "./model";

const PAD = { l: 58, r: 12, t: 12, b: 24 } as const;
const H = 230;
/** How near (px) the pointer must be to a mark to pick it. */
const REACH = 16;

export function DurationScatter({
  traces,
  start,
  end,
  onOpen,
}: {
  traces: TraceSummary[];
  start: number;
  end: number;
  onOpen: (traceId: string) => void;
}) {
  const frame = useRef<HTMLDivElement>(null);
  const [W, setW] = useState(960);
  useLayoutEffect(() => {
    const el = frame.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(([e]) => setW(Math.max(280, Math.round(e.contentRect.width))));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const [hover, setHover] = useState<string | null>(null);

  const geo = useMemo(() => {
    const durs = traces.map((t) => Math.max(t.durationMs, 0.01));
    let a = Math.floor(Math.log10(Math.min(...durs, 1)));
    let b = Math.ceil(Math.log10(Math.max(...durs, 1)));
    if (b <= a) b = a + 1;
    if (b - a > 7) a = b - 7;
    const iw = W - PAD.l - PAD.r;
    const ih = H - PAD.t - PAD.b;
    const span = Math.max(1, end - start);
    const x = (t: number) => PAD.l + Math.min(1, Math.max(0, (t - start) / span)) * iw;
    const y = (d: number) => PAD.t + (1 - (Math.log10(Math.max(d, 10 ** a)) - a) / (b - a)) * ih;
    const pts = traces.map((t) => ({ t, x: x(t.start), y: y(Math.max(t.durationMs, 0.01)), err: t.errors > 0 }));
    const yTicks: number[] = [];
    for (let k = a; k <= b; k++) yTicks.push(10 ** k);
    const n = W < 520 ? 3 : 5;
    const xTicks = Array.from({ length: n }, (_, i) => start + (span * i) / (n - 1));
    return { pts, y, x, yTicks, xTicks };
  }, [traces, start, end, W]);

  const nearest = (e: MouseEvent<SVGSVGElement>): string | null => {
    const r = e.currentTarget.getBoundingClientRect();
    const px = ((e.clientX - r.left) / r.width) * W;
    const py = ((e.clientY - r.top) / r.height) * H;
    let best: string | null = null;
    let bd = REACH * REACH;
    for (const p of geo.pts) {
      const d = (p.x - px) ** 2 + (p.y - py) ** 2;
      if (d < bd) {
        bd = d;
        best = p.t.traceId;
      }
    }
    return best;
  };

  const hovered = hover ? geo.pts.find((p) => p.t.traceId === hover) : undefined;
  const errCount = geo.pts.filter((p) => p.err).length;

  return (
    <div>
      <div ref={frame} className="frame-draw wipe relative border border-border">
        <svg
          viewBox={`0 0 ${W} ${H}`}
          width="100%"
          height={H}
          className={`block touch-manipulation ${hovered ? "cursor-pointer" : ""}`}
          role="img"
          aria-label={`Trace durations over time: ${plural(traces.length, "trace")}, ${plural(errCount, "with errors", "with errors")}. The table below lists the same traces.`}
          onPointerMove={(e) => setHover(nearest(e))}
          onPointerLeave={() => setHover(null)}
          onClick={(e) => {
            const id = nearest(e);
            if (id) onOpen(id);
          }}
        >
          {geo.yTicks.map((v) => (
            <g key={v}>
              <line x1={PAD.l} x2={W - PAD.r} y1={geo.y(v)} y2={geo.y(v)} className={chart.grid} />
              <text x={PAD.l - 8} y={geo.y(v)} dy="0.32em" textAnchor="end" className={chart.tick}>
                {fmtMs(v)}
              </text>
            </g>
          ))}
          {geo.xTicks.map((t, i) => (
            <text
              key={t}
              x={geo.x(t)}
              y={H - 6}
              textAnchor={i === 0 ? "start" : i === geo.xTicks.length - 1 ? "end" : "middle"}
              className={chart.tick}
            >
              {clock(t)}
            </text>
          ))}
          {geo.pts.map((p) =>
            p.err ? null : <circle key={p.t.traceId} cx={p.x} cy={p.y} r={2.4} className="fill-foreground/55" />,
          )}
          {geo.pts.map((p) =>
            p.err ? <rect key={p.t.traceId} x={p.x - 3.5} y={p.y - 3.5} width={7} height={7} className="fill-tone-critical" /> : null,
          )}
          {hovered && (
            <g pointerEvents="none">
              <line x1={hovered.x} x2={hovered.x} y1={PAD.t} y2={H - PAD.b} className="stroke-foreground/40" strokeDasharray="2 3" />
              <rect x={hovered.x - 7} y={hovered.y - 7} width={14} height={14} className="fill-none stroke-foreground" />
            </g>
          )}
        </svg>
        <div className="scan" aria-hidden="true" />
      </div>
      <div className={`${typography.eyebrow} mt-2 flex flex-wrap items-baseline justify-between gap-x-5 gap-y-1`}>
        <span className="min-w-0 [overflow-wrap:anywhere]" aria-live="polite">
          {hovered ? (
            <span className="text-foreground">
              {hovered.t.rootService} · {hovered.t.rootName} · {fmtMs(hovered.t.durationMs)} · {clock(hovered.t.start)}
              {hovered.t.errors > 0 ? ` · ${plural(hovered.t.errors, "error")}` : ""} · select to open
            </span>
          ) : (
            "point at a mark for its trace · select it to open · duration on a log scale"
          )}
        </span>
        <span className="inline-flex flex-wrap items-center gap-x-4 gap-y-1" aria-hidden="true">
          <span className="inline-flex items-center gap-2">
            <span className="inline-block h-[5px] w-[5px] rounded-full bg-foreground/55" />
            trace
          </span>
          <span className="inline-flex items-center gap-2">
            <span className="inline-block h-2 w-2 bg-tone-critical" />
            trace with errors
          </span>
        </span>
      </div>
    </div>
  );
}
