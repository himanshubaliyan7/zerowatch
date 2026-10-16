/**
 * The signature element: a request seismograph. Every request that reaches
 * the log stream with a status and a duration is one vertical tick, drawn
 * where it happened in the last 60 seconds. Height is latency on a log scale
 * (1 ms to 10 s); colour is the HTTP class: ink for 2xx/3xx, amber for 4xx,
 * red for 5xx. The dashed line is the lowest latency alarm threshold.
 *
 * The chart only moves because data arrives: it redraws once a second.
 */
import { useEffect, useRef, useState } from "react";
import { endpoints } from "@/lib/api";
import { parseRequest, subscribeLive, type RequestSample } from "@/lib/live";
import { useNow } from "@/lib/hooks";
import { num } from "@/lib/format";
import { chart, typography } from "@/design/tokens";

const SPAN_MS = 60_000;
const W = 1000;
const H = 240;
const BASE = H - 20;
const TOP = 22;
const MAX_TICKS = 6000;

const y = (latency: number) => BASE - (Math.log10(Math.min(10_000, Math.max(1, latency))) / 4) * (BASE - TOP);

export function Seismograph({ thresholdMs }: { thresholdMs?: number }) {
  const ticks = useRef<RequestSample[]>([]);
  const [loaded, setLoaded] = useState(false);
  const now = useNow(1000);

  useEffect(() => {
    const ctrl = new AbortController();
    endpoints
      .searchLogs({ start: Date.now() - SPAN_MS, limit: 5000 }, ctrl.signal)
      .then((r) => {
        const past = r.events.map(parseRequest).filter((s): s is RequestSample => s != null);
        ticks.current = [...past.reverse(), ...ticks.current].slice(-MAX_TICKS);
      })
      .catch(() => {
        // Live events still fill the chart.
      })
      .finally(() => setLoaded(true));
    const off = subscribeLive((ev) => {
      if (ev.kind !== "log") return;
      const s = parseRequest(ev.log);
      if (s) {
        ticks.current.push(s);
        if (ticks.current.length > MAX_TICKS) ticks.current.splice(0, ticks.current.length - MAX_TICKS);
      }
    });
    return () => {
      ctrl.abort();
      off();
    };
  }, []);

  const from = now - SPAN_MS;
  const live = ticks.current.filter((k) => k.t >= from && k.t <= now + 2000);
  ticks.current = ticks.current.filter((k) => k.t >= from);
  let ok = "";
  let client = "";
  let server = "";
  let n4 = 0;
  let n5 = 0;
  for (const k of live) {
    const x = (((k.t - from) / SPAN_MS) * W).toFixed(1);
    const seg = `M${x} ${BASE}V${y(k.latencyMs).toFixed(1)}`;
    if (k.status >= 500) {
      server += seg;
      n5++;
    } else if (k.status >= 400) {
      client += seg;
      n4++;
    } else ok += seg;
  }
  const rate = live.length / (SPAN_MS / 1000);
  const empty = loaded && live.length === 0;

  return (
    <div>
      <div className="plot-grid frame-draw relative h-60 overflow-hidden border border-border sm:h-72">
        <span className="scan" aria-hidden="true" />
        <svg
          viewBox={`0 0 ${W} ${H}`}
          preserveAspectRatio="none"
          className="absolute inset-0 h-full w-full"
          role="img"
          aria-label={`Requests in the last 60 seconds: ${live.length} logged, ${n4} client errors, ${n5} server errors.`}
        >
          {[1, 10, 100, 1000].map((v) => (
            <line key={v} x1={0} x2={W} y1={y(v)} y2={y(v)} className={chart.grid} vectorEffect="non-scaling-stroke" />
          ))}
          <line x1={0} x2={W} y1={BASE} y2={BASE} stroke="var(--border)" vectorEffect="non-scaling-stroke" />
          {thresholdMs != null && (
            <line x1={0} x2={W} y1={y(thresholdMs)} y2={y(thresholdMs)} className={chart.threshold} strokeWidth={1} vectorEffect="non-scaling-stroke" />
          )}
          <path d={ok} stroke="var(--foreground)" strokeOpacity={0.55} strokeWidth={1} vectorEffect="non-scaling-stroke" />
          <path d={client} stroke="var(--tone-warning)" strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
          <path d={server} stroke="var(--tone-critical)" strokeWidth={2} vectorEffect="non-scaling-stroke" />
        </svg>
        <div className={`${typography.eyebrow} pointer-events-none absolute inset-x-2.5 top-2 flex justify-between`} aria-hidden="true">
          <span>10 s</span>
          <span className="text-foreground">{num(rate, rate < 10 ? 1 : 0)} logged req/s</span>
        </div>
        {[1000, 100, 10].map((v) => (
          <span key={v} className={`${typography.eyebrow} pointer-events-none absolute left-1.5 bg-background/85 px-1`} style={{ top: `calc(${(y(v) / H) * 100}% - 0.55rem)` }} aria-hidden="true">
            {v >= 1000 ? "1 s" : `${v} ms`}
          </span>
        ))}
        <div className={`${typography.eyebrow} pointer-events-none absolute inset-x-2.5 bottom-1 flex justify-between`} aria-hidden="true">
          <span>−60 s</span>
          <span>−30 s</span>
          <span>now</span>
        </div>
        {empty && (
          <div className="hatch absolute inset-0 flex items-center justify-center p-6 text-center">
            <p className="max-w-[46ch] bg-background px-3 py-2 text-[0.95rem] leading-relaxed">
              No request lines in the last minute. The seismograph draws log lines that carry a status and a duration (JSON
              <span className="font-mono text-[0.8rem]"> status / duration_ms</span>, or <span className="font-mono text-[0.8rem]">200 31ms</span>).
            </p>
          </div>
        )}
      </div>
      <div className={`${typography.micro} mt-3 flex flex-wrap gap-x-6 gap-y-1.5`}>
        <span className="inline-flex items-center gap-2">
          <span className="inline-block h-2.5 w-2.5 bg-foreground" aria-hidden="true" />
          2xx / 3xx
        </span>
        <span className="inline-flex items-center gap-2">
          <span className="inline-block h-2.5 w-2.5 bg-tone-warning" aria-hidden="true" />
          4xx client error · {num(n4)}
        </span>
        <span className="inline-flex items-center gap-2">
          <span className="inline-block h-2.5 w-2.5 bg-tone-critical" aria-hidden="true" />
          5xx server error · {num(n5)}
        </span>
        {thresholdMs != null && (
          <span className="inline-flex items-center gap-2 text-muted-foreground">
            <span className={chart.swatch.threshold} aria-hidden="true" />
            p99 alarm threshold · {num(thresholdMs)} ms
          </span>
        )}
      </div>
    </div>
  );
}
