/**
 * The shared time-series plot (hand-drawn SVG, no chart library). Several
 * series share one time axis; each differs by dash pattern as well as shade,
 * so nothing relies on colour. Optional: an uncertainty band (anomaly
 * detection), marked anomalous points (the only colour: a status), a dashed
 * threshold, and a click handler that reports the time under the pointer
 * (used for "logs for this moment"). A forecast continues the first series
 * past a NOW rule: dashed, with its interval as a band, and a square in the
 * warning tone where it first crosses the threshold.
 *
 * Gaps (null) break the line; they are never drawn as zero.
 */
import { useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { chart, typography } from "@/design/tokens";
import { clock, num } from "@/lib/format";

export interface PlotSeries {
  label: string;
  t: number[];
  v: (number | null)[];
}

export interface PlotBand {
  t: number[];
  lower: (number | null)[];
  upper: (number | null)[];
  /** indices of points outside the band, drawn as red squares */
  anomalous?: number[];
}

/** Future periods after the last observed point (POST /api/v1/forecast points). */
export interface PlotForecast {
  t: number[];
  forecast: number[];
  lower: number[];
  upper: number[];
  /** first forecast time breaching the threshold, if any */
  crossesAt?: number | null;
}

/**
 * Inner padding of the plot area in CSS pixels (the drawing is laid out at
 * its real width). Things aligned under the time axis, like the alarm
 * simulator's state strip, use these as pixel margins.
 */
export const PLOT_PAD = { l: 48, r: 12, t: 12, b: 24 } as const;

/** Dash patterns, in order of series. Shade steps from ink to muted. */
const DASH = ["", "6 4", "2 3", "10 3 2 3", "1 4"];
const SHADE = ["stroke-foreground", "stroke-foreground/75", "stroke-muted-foreground", "stroke-foreground/60", "stroke-muted-foreground"];

export function seriesStyle(i: number): { dash: string; shade: string } {
  return { dash: DASH[i % DASH.length], shade: SHADE[i % SHADE.length] };
}

export function MultiPlot({
  series,
  band,
  forecast,
  threshold,
  height = 220,
  unit,
  onPick,
  pickLabel = "Logs for this moment",
  ariaLabel,
}: {
  series: PlotSeries[];
  band?: PlotBand;
  forecast?: PlotForecast;
  threshold?: number;
  height?: number;
  unit?: string;
  /** Called with the time (ms) of the nearest point when the plot is clicked. */
  onPick?: (t: number) => void;
  pickLabel?: string;
  ariaLabel: string;
}) {
  // The drawing is laid out at the frame's real pixel width, so ticks and
  // labels keep their 10 px size in narrow columns and on phones.
  const frame = useRef<HTMLDivElement>(null);
  const [W, setW] = useState(960);
  useLayoutEffect(() => {
    const el = frame.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(([e]) => setW(Math.max(280, Math.round(e.contentRect.width))));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const H = height;
  const pad = PLOT_PAD;
  const clipId = useId();
  const [hover, setHover] = useState<number | null>(null);

  const { t0, t1, hi, times } = useMemo(() => {
    const ts = new Set<number>();
    let max = threshold ?? 0;
    for (const s of series)
      s.t.forEach((t, i) => {
        ts.add(t);
        const v = s.v[i];
        if (v != null && Number.isFinite(v)) max = Math.max(max, v);
      });
    band?.upper.forEach((u) => {
      if (u != null && Number.isFinite(u)) max = Math.max(max, u);
    });
    forecast?.t.forEach((t, i) => {
      ts.add(t);
      if (Number.isFinite(forecast.upper[i])) max = Math.max(max, forecast.upper[i]);
    });
    const times = [...ts].sort((a, b) => a - b);
    return { t0: times[0] ?? 0, t1: times[times.length - 1] ?? 1, hi: (max || 1) * 1.1, times };
  }, [series, band, forecast, threshold]);

  const x = (t: number) => pad.l + ((t - t0) / Math.max(1, t1 - t0)) * (W - pad.l - pad.r);
  const y = (v: number) => H - pad.b - (Math.max(0, v) / hi) * (H - pad.t - pad.b);
  const empty = series.every((s) => s.v.every((v) => v == null));

  const line = (s: PlotSeries) => {
    let d = "";
    let pen = false;
    s.t.forEach((t, i) => {
      const v = s.v[i];
      if (v == null || !Number.isFinite(v)) {
        pen = false;
        return;
      }
      d += `${pen ? "L" : "M"}${x(t).toFixed(1)} ${y(v).toFixed(1)}`;
      pen = true;
    });
    return d;
  };

  const bandPath = useMemo(() => {
    if (!band) return "";
    // One closed shape per unbroken run of defined points.
    let d = "";
    let run: number[] = [];
    const flush = () => {
      if (run.length > 1) {
        const top = run.map((i, k) => `${k ? "L" : "M"}${x(band.t[i]).toFixed(1)} ${y(band.upper[i]!).toFixed(1)}`).join("");
        const bottom = [...run].reverse().map((i) => `L${x(band.t[i]).toFixed(1)} ${y(band.lower[i]!).toFixed(1)}`).join("");
        d += `${top}${bottom}Z`;
      }
      run = [];
    };
    band.t.forEach((_, i) => {
      if (band.lower[i] != null && band.upper[i] != null) run.push(i);
      else flush();
    });
    flush();
    return d;
    // x/y depend on the memoised extent above
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [band, t0, t1, hi, W]);

  // The forecast starts from the last observed point of the first series, so
  // the dashed line and its band join the solid one at NOW.
  const fc = useMemo(() => {
    if (!forecast?.t.length) return null;
    const s = series[0];
    let k = s ? s.v.length - 1 : -1;
    while (k >= 0 && (s.v[k] == null || !Number.isFinite(s.v[k]!))) k--;
    const now = k >= 0 ? s.t[k] : forecast.t[0];
    const lastV = k >= 0 ? s.v[k]! : forecast.forecast[0];
    const pts = forecast.t.map((t, i) => [x(t), y(forecast.forecast[i]), y(forecast.lower[i]), y(forecast.upper[i])] as const);
    const start = `${x(now).toFixed(1)} ${y(lastV).toFixed(1)}`;
    const lineD = `M${start}` + pts.map(([px, py]) => `L${px.toFixed(1)} ${py.toFixed(1)}`).join("");
    const bandD =
      `M${start}` +
      pts.map(([px, , , pu]) => `L${px.toFixed(1)} ${pu.toFixed(1)}`).join("") +
      [...pts].reverse().map(([px, , pl]) => `L${px.toFixed(1)} ${pl.toFixed(1)}`).join("") +
      "Z";
    return { now, lineD, bandD };
    // x/y depend on the memoised extent above
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [forecast, series, t0, t1, hi, W]);
  const fcAt = (t: number) => (forecast ? forecast.t.indexOf(t) : -1);

  const ticks = [0, hi / 2, hi];
  const fmt = (v: number) => (v >= 10_000 ? `${num(v / 1000, 0)}k` : v >= 1000 ? `${num(v / 1000, 1)}k` : num(v, v < 10 && v > 0 ? 1 : 0));
  // One point (a fresh node) has no span: one tick, not three on top of each other.
  const xTicks = !times.length ? [] : t1 > t0 ? [t0, t0 + (t1 - t0) / 2, t1] : [t0];

  const nearest = (clientX: number, rect: DOMRect) => {
    const px = ((clientX - rect.left) / rect.width) * W;
    const t = t0 + ((px - pad.l) / Math.max(1, W - pad.l - pad.r)) * (t1 - t0);
    let best = times[0];
    for (const tt of times) if (Math.abs(tt - t) < Math.abs(best - t)) best = tt;
    return best;
  };

  if (empty) {
    return (
      <div ref={frame} className="hatch flex items-center justify-center border border-border p-6 text-center text-[0.95rem] text-muted-foreground" style={{ height }}>
        No data in this window.
      </div>
    );
  }

  return (
    <div ref={frame} className="relative border border-border">
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className={`block h-auto w-full ${onPick ? "cursor-crosshair" : ""}`}
        role="img"
        aria-label={ariaLabel}
        onMouseMove={(e) => setHover(nearest(e.clientX, e.currentTarget.getBoundingClientRect()))}
        onMouseLeave={() => setHover(null)}
        onClick={onPick ? (e) => onPick(nearest(e.clientX, e.currentTarget.getBoundingClientRect())) : undefined}
      >
        <defs>
          <clipPath id={clipId}>
            <rect x={pad.l} y={pad.t} width={W - pad.l - pad.r} height={H - pad.t - pad.b} />
          </clipPath>
        </defs>
        {ticks.map((v) => (
          <g key={v}>
            <line x1={pad.l} x2={W - pad.r} y1={y(v)} y2={y(v)} className={chart.grid} />
            <text x={pad.l - 6} y={y(v) + 3} textAnchor="end" className={chart.tick}>
              {fmt(v)}
            </text>
          </g>
        ))}
        {xTicks.map((t, i) => (
          <text key={i} x={x(t)} y={H - 6} textAnchor={i === 0 ? "start" : i === 2 ? "end" : "middle"} className={chart.tick}>
            {new Date(t).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" })}
          </text>
        ))}
        <g clipPath={`url(#${clipId})`}>
          {bandPath && <path d={bandPath} className="fill-foreground/12" />}
          {fc && (
            <>
              <line x1={x(fc.now)} x2={x(fc.now)} y1={pad.t} y2={H - pad.b} className="stroke-border" />
              <path d={fc.bandD} className="wipe fill-foreground/12" />
              <path d={fc.lineD} className="wipe fill-none stroke-foreground" strokeWidth={2} strokeDasharray="6 4" strokeLinejoin="round" />
            </>
          )}
          {threshold != null && <line x1={pad.l} x2={W - pad.r} y1={y(threshold)} y2={y(threshold)} className={chart.threshold} strokeWidth={1} />}
          {series.map((s, i) => {
            const st = seriesStyle(i);
            return <path key={s.label + i} d={line(s)} className={`wipe fill-none ${st.shade}`} strokeWidth={i === 0 ? 2 : 1.5} strokeDasharray={st.dash || undefined} strokeLinejoin="round" />;
          })}
          {band?.anomalous?.map((i) => {
            const s = series[0];
            const v = s?.v[s.t.indexOf(band.t[i])];
            return v == null ? null : <rect key={i} className="pop fill-tone-critical" x={x(band.t[i]) - 3} y={y(v) - 3} width={6} height={6} />;
          })}
          {fc && threshold != null && forecast?.crossesAt != null && (
            <rect className="pop fill-tone-warning" x={x(forecast.crossesAt) - 4} y={y(threshold) - 4} width={8} height={8} />
          )}
          {hover != null && <line x1={x(hover)} x2={x(hover)} y1={pad.t} y2={H - pad.b} className="stroke-foreground/40" strokeDasharray="2 3" />}
        </g>
        {fc && (
          <text x={x(fc.now) + 5} y={pad.t + 10} className={chart.tick}>
            now
          </text>
        )}
        {fc && threshold != null && forecast?.crossesAt != null && (
          <text
            x={Math.min(x(forecast.crossesAt) + 8, W - pad.r - 4)}
            y={Math.max(pad.t + 22, y(threshold) - 8)}
            textAnchor={x(forecast.crossesAt) + 8 > W - pad.r - 110 ? "end" : "start"}
            className={chart.tick}
          >
            crosses {clock(forecast.crossesAt).slice(0, 5)}
          </text>
        )}
      </svg>
      {hover != null && (
        <div className={`${typography.micro} pointer-events-none absolute right-2 top-2 border border-border bg-background px-2 py-1`}>
          <div className="text-muted-foreground">{clock(hover)}</div>
          {fcAt(hover) >= 0 && forecast && (
            <div className="tabular-nums">
              forecast: {num(forecast.forecast[fcAt(hover)], forecast.forecast[fcAt(hover)] < 10 ? 2 : 0)}
              {unit ? ` ${unit}` : ""} ({num(forecast.lower[fcAt(hover)], 0)}–{num(forecast.upper[fcAt(hover)], 0)})
            </div>
          )}
          {series.slice(0, 4).map((s) => {
            if (fcAt(hover) >= 0 && s.t.indexOf(hover) < 0) return null;
            const v = s.v[s.t.indexOf(hover)];
            return (
              <div key={s.label} className="tabular-nums">
                {s.label}: {v == null ? "—" : num(v, v < 10 ? 2 : 0)}
                {unit ? ` ${unit}` : ""}
              </div>
            );
          })}
          {onPick && <div className="text-muted-foreground">click: {pickLabel.toLowerCase()}</div>}
        </div>
      )}
    </div>
  );
}

/** Legend with line swatches matching the plot's dash patterns. */
export function PlotLegend({ labels, band, threshold, forecast }: { labels: string[]; band?: string; threshold?: string; forecast?: string }) {
  return (
    <div className={`${typography.micro} mt-2.5 flex flex-wrap gap-x-5 gap-y-1.5`}>
      {labels.map((l, i) => {
        const st = seriesStyle(i);
        return (
          <span key={l + i} className="inline-flex items-center gap-2">
            <svg width="20" height="6" aria-hidden="true">
              <line x1="0" x2="20" y1="3" y2="3" className={st.shade} strokeWidth={2} strokeDasharray={st.dash || undefined} />
            </svg>
            {l}
          </span>
        );
      })}
      {band && (
        <span className="inline-flex items-center gap-2 text-muted-foreground">
          <span className="inline-block h-3 w-5 bg-foreground/12" aria-hidden="true" />
          {band}
        </span>
      )}
      {forecast && (
        <span className="inline-flex items-center gap-2">
          <svg width="20" height="6" aria-hidden="true">
            <line x1="0" x2="20" y1="3" y2="3" className="stroke-foreground" strokeWidth={2} strokeDasharray="6 4" />
          </svg>
          {forecast}
        </span>
      )}
      {threshold && (
        <span className="inline-flex items-center gap-2 text-muted-foreground">
          <span className={chart.swatch.threshold} aria-hidden="true" />
          {threshold}
        </span>
      )}
    </div>
  );
}
