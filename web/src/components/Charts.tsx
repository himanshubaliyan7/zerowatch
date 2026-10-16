/**
 * Hand-drawn SVG charts (no chart library): a sparkline for table rows and a
 * small framed line plot for a service's RED board. Missing points break the
 * line rather than being drawn as zero.
 */
import { stagger } from "@/design/primitives";
import { chart, typography } from "@/design/tokens";
import { num } from "@/lib/format";

type Pt = number | null;

function path(values: Pt[], x: (i: number) => number, y: (v: number) => number): string {
  let d = "";
  let pen = false;
  values.forEach((v, i) => {
    if (v == null || !Number.isFinite(v)) {
      pen = false;
      return;
    }
    d += `${pen ? "L" : "M"}${x(i).toFixed(1)} ${y(v).toFixed(1)}`;
    pen = true;
  });
  return d;
}

export function Sparkline({ values, index = 0, label }: { values: Pt[]; index?: number; label: string }) {
  const real = values.filter((v): v is number => v != null);
  if (real.length < 2) {
    return <div className="hatch h-7 w-36 border border-dashed border-border" role="img" aria-label={`${label}: no data`} />;
  }
  const W = 144;
  const H = 28;
  const lo = Math.min(...real);
  const hi = Math.max(...real);
  const y = (v: number) => H - 3 - ((v - lo) / (hi - lo || 1)) * (H - 6);
  const x = (i: number) => (i / Math.max(1, values.length - 1)) * W;
  return (
    <svg className="wipe block h-7 w-36" style={stagger(index)} viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label={`${label}: ${num(lo)} to ${num(hi)}`}>
      <path d={path(values, x, y)} className={chart.line.primary} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

/**
 * A framed per-minute plot with a hairline grid, mono ticks, the series in
 * solid ink and an optional dashed threshold.
 */
export function MiniPlot({
  title,
  unit,
  t,
  values,
  threshold,
  decimals = 0,
}: {
  title: string;
  unit: string;
  t: number[];
  values: Pt[];
  threshold?: number;
  decimals?: number;
}) {
  const W = 480;
  const H = 150;
  const pad = { l: 40, r: 8, t: 10, b: 20 };
  const real = values.filter((v): v is number => v != null);
  const hi = Math.max(1, threshold ?? 0, ...real) * 1.1;
  const x = (i: number) => pad.l + (i / Math.max(1, values.length - 1)) * (W - pad.l - pad.r);
  const y = (v: number) => H - pad.b - (v / hi) * (H - pad.t - pad.b);
  const ticks = [0, hi / 2, hi];
  const fmt = (v: number) => (v >= 1000 ? `${num(v / 1000, 1)}k` : num(v, v < 10 ? decimals : 0));
  return (
    <figure className="min-w-0">
      <figcaption className={`${typography.micro} mb-2 flex justify-between gap-3`}>
        <span>{title}</span>
        <span className="text-muted-foreground">{unit}</span>
      </figcaption>
      <div className="relative border border-border">
        {real.length === 0 ? (
          <div className="hatch flex h-[150px] items-center justify-center text-sm text-muted-foreground">No data in this window</div>
        ) : (
          <svg viewBox={`0 0 ${W} ${H}`} className="block h-auto w-full" role="img" aria-label={`${title}, ${unit}`}>
            {ticks.map((v) => (
              <g key={v}>
                <line x1={pad.l} x2={W - pad.r} y1={y(v)} y2={y(v)} className={chart.grid} />
                <text x={pad.l - 6} y={y(v) + 3} textAnchor="end" className={chart.tick}>
                  {fmt(v)}
                </text>
              </g>
            ))}
            {[0, Math.floor((t.length - 1) / 2), t.length - 1].map((i) => (
              <text key={i} x={x(i)} y={H - 5} textAnchor={i === 0 ? "start" : i === t.length - 1 ? "end" : "middle"} className={chart.tick}>
                {new Date(t[i]).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" })}
              </text>
            ))}
            {threshold != null && <line x1={pad.l} x2={W - pad.r} y1={y(threshold)} y2={y(threshold)} className={chart.threshold} strokeWidth={1} />}
            <path d={path(values, x, y)} className={`${chart.line.primary} wipe`} strokeWidth={2} />
          </svg>
        )}
      </div>
    </figure>
  );
}
