/**
 * Auto-RED: every service the platform has seen, with no dashboard to build.
 * Rate, errors and p99 duration per service over the last 15 minutes, the
 * live request seismograph, and one click from a service (or a moment on its
 * RED board) to its logs.
 */
import { Fragment, useMemo, useState, type ReactNode } from "react";
import { endpoints, RED_NAMESPACE, type AlarmRule, type MetricDataQuery, type QueryResult } from "@/lib/api";
import { usePoll } from "@/lib/hooks";
import { fetchRed, listServices, DEGRADED_ERROR_PCT, WINDOW_MIN, type ServiceRed } from "@/lib/red";
import { num, plural } from "@/lib/format";
import { Empty, ErrorNote, PageHead, Section, Skeleton, Stat, StatGrid, StatusMark } from "@/design/primitives";
import { control, typography } from "@/design/tokens";
import { Sparkline } from "@/components/Charts";
import { MultiPlot, PlotLegend, type PlotSeries } from "@/components/MultiPlot";
import { logsLink } from "@/lib/range";
import { Seismograph } from "@/components/Seismograph";

export function Overview() {
  const services = usePoll(() => listServices(), 30_000);
  const rules = usePoll(() => endpoints.alarms(), 10_000);
  const names = services.data ?? [];
  const red = usePoll((signal) => fetchRed(names, rules.data ?? [], signal), 10_000, [names.join(","), rules.data]);
  const [open, setOpen] = useState<string | null>(null);

  const firing = (rules.data ?? []).filter((r) => r.state === "ALARM");
  const threshold = useMemo(() => lowestLatencyThreshold(rules.data ?? []), [rules.data]);

  const headline = [
    services.data ? `${plural(names.length, "service")}.` : "Looking…",
    rules.data ? (firing.length ? `${plural(firing.length, "alarm")} firing.` : "Nothing firing.") : " ",
  ];

  return (
    <div className="space-y-12 sm:space-y-16">
      <PageHead
        mega
        eyebrow={`Auto-RED · ${plural(names.length, "service")} discovered · last ${WINDOW_MIN} min`}
        lines={headline}
        lead="Every request the probe and the logs saw in the last minute, drawn as it happened. Nothing was installed in the apps: the services were found on their own."
      />

      <Section index={3} title="Request seismograph · last 60 s" note="one tick per logged request · height = latency">
        <Seismograph thresholdMs={threshold} />
      </Section>

      <Section index={4} title={`Totals · ${WINDOW_MIN} min`} note="all services">
        {red.error && !red.data ? (
          <ErrorNote error={red.error} what="RED totals" />
        ) : (
          <StatGrid>
            <Stat label="Rate" value={red.data?.totals.rps ?? null} decimals={red.data && (red.data.totals.rps ?? 0) < 10 ? 1 : 0} unit="requests / s" />
            <Stat label="Errors" value={red.data?.totals.errorPct ?? null} decimals={1} unit="% of requests 5xx" />
            <Stat label="Duration p50" value={red.data?.totals.p50 ?? null} unit="milliseconds" />
            <Stat label="Duration p99" value={red.data?.totals.p99 ?? null} unit="milliseconds" />
          </StatGrid>
        )}
      </Section>

      <Section
        index={5}
        title="Services"
        note={
          <span className="inline-flex flex-wrap gap-x-4 gap-y-1">
            <span>degraded = 5xx ≥ {DEGRADED_ERROR_PCT} %</span>
            <span>select a row for its RED board</span>
          </span>
        }
      >
        {services.error && !services.data ? (
          <ErrorNote error={services.error} what="services" />
        ) : !services.data || (names.length > 0 && !red.data) ? (
          <div className="space-y-2">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-12 w-full" />
            ))}
          </div>
        ) : names.length === 0 ? (
          <Empty hatch>
            No services yet. Services appear here once anything writes RED metrics: the supervisor's probe, the synthetic generator
            (<span className="font-mono text-[0.8rem]">--generate=true</span>), or an app sending OTLP or CloudWatch data.
          </Empty>
        ) : (
          <ServiceTable rows={red.data?.services ?? []} open={open} onToggle={(n) => setOpen(open === n ? null : n)} rules={rules.data ?? []} />
        )}
      </Section>
    </div>
  );
}

function lowestLatencyThreshold(rules: AlarmRule[], service?: string): number | undefined {
  const t = rules
    .filter((r) => r.enabled && r.metric?.metricName === "Latency" && r.config.comparison.startsWith("Greater"))
    .filter((r) => service == null || r.metric?.dimensions?.Service === service)
    .map((r) => r.config.threshold);
  return t.length ? Math.min(...t) : undefined;
}

function ServiceTable({ rows, open, onToggle, rules }: { rows: ServiceRed[]; open: string | null; onToggle: (n: string) => void; rules: AlarmRule[] }) {
  return (
    <div className="relative overflow-x-auto">
      <table className="w-full min-w-[40rem] border-collapse text-left">
        <thead>
          <tr className={`${typography.eyebrow}`}>
            <th className="pb-2.5 pr-3 font-medium">Service</th>
            <th className="pb-2.5 pr-3 font-medium">Status</th>
            <th className="pb-2.5 pr-3 text-right font-medium">req/s</th>
            <th className="pb-2.5 pr-3 text-right font-medium">5xx %</th>
            <th className="pb-2.5 pr-3 text-right font-medium">p99 ms</th>
            <th className="pb-2.5 font-medium">p99 · {WINDOW_MIN} min</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((s, i) => {
            const isOpen = open === s.name;
            return (
              <Fragment key={s.name}>
                <tr
                  className="group cursor-pointer border-t border-hair transition-colors hover:bg-muted"
                  onClick={() => onToggle(s.name)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      onToggle(s.name);
                    }
                  }}
                  tabIndex={0}
                  aria-expanded={isOpen}
                >
                  <td className="py-3 pr-3 transition-[padding] duration-200 group-hover:pl-3">
                    <span className={typography.rowName}>{s.name}</span>
                  </td>
                  <td className="py-3 pr-3">
                    <StatusMark tone={s.tone} label={s.status} />
                  </td>
                  <td className={`py-3 pr-3 text-right ${typography.number}`}>{num(s.rps, s.rps != null && s.rps < 10 ? 1 : 0)}</td>
                  <td className={`py-3 pr-3 text-right ${typography.number}`}>{num(s.errorPct, 1)}</td>
                  <td className={`py-3 pr-3 text-right ${typography.number}`}>{num(s.p99)}</td>
                  <td className="py-3">
                    <Sparkline values={s.minutes.p99} index={i} label={`${s.name} p99 latency`} />
                  </td>
                </tr>
                {isOpen && (
                  <tr className="border-t border-hair">
                    <td colSpan={6} className="pb-8 pt-4">
                      <ServiceBoard s={s} threshold={lowestLatencyThreshold(rules, s.name)} />
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

const WEEK_MS = 7 * 24 * 60 * 60_000;
const PERCENTILES = ["p50", "p90", "p95", "p99"] as const;

/** Align a result to a minute grid (shifted by `shift` ms); missing minutes stay null, never zero. */
function onGrid(r: QueryResult | undefined, grid: number[], shift = 0): (number | null)[] {
  if (!r) return grid.map(() => null);
  const byT = new Map<number, number>();
  r.timestamps.forEach((t, i) => byT.set(Math.floor((t + shift) / 60_000) * 60_000, r.values[i]));
  return grid.map((t) => byT.get(t) ?? null);
}

/** Duration percentiles for this window, and the request rate of the same window a week earlier. */
async function fetchBoard(name: string, grid: number[], signal: AbortSignal) {
  const start = grid[0];
  const end = grid[grid.length - 1] + 60_000;
  const d = { Service: name, Route: "*" };
  const metric = (id: string, metricName: string, stat: string): MetricDataQuery => ({ id, metricStat: { namespace: RED_NAMESPACE, metricName, stat, dimensions: d } });
  const [now, week] = await Promise.all([
    endpoints.query({ queries: PERCENTILES.map((p) => metric(p, "Latency", p)), start, end, period: 60 }, signal),
    endpoints.query({ queries: [metric("lw", "Requests", "Sum")], start: start - WEEK_MS, end: end - WEEK_MS, period: 60 }, signal),
  ]);
  const byId = new Map(now.map((r) => [r.id, r]));
  return {
    percentiles: PERCENTILES.map((p) => ({ label: p, t: grid, v: onGrid(byId.get(p), grid) })),
    lastWeek: onGrid(week.find((r) => r.id === "lw"), grid, WEEK_MS),
  };
}

function ServiceBoard({ s, threshold }: { s: ServiceRed; threshold?: number }) {
  const q = encodeURIComponent(`service=${s.name}`);
  const grid = s.minutes.t;
  const board = usePoll((signal) => fetchBoard(s.name, grid, signal), 30_000, [s.name, grid[0]]);
  const pick = (t: number) => {
    window.location.hash = logsLink(t - 30_000, t + 60_000, `service=${s.name}`);
  };

  const lastWeek = board.data?.lastWeek ?? [];
  const hasLastWeek = lastWeek.some((v) => v != null);
  const rate: PlotSeries[] = [{ label: "this window", t: grid, v: s.minutes.req }];
  if (hasLastWeek) rate.push({ label: "last week", t: grid, v: lastWeek });
  const errPct = s.minutes.req.map((r, i) => {
    const e = s.minutes.err[i];
    return r == null || r <= 0 ? null : (100 * (e ?? 0)) / r;
  });

  return (
    <div className="row-in space-y-5">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <p className={typography.eyebrow}>
          RED board · {s.name} · per minute · select a point to see its logs
          {s.alarms.length > 0 && <> · firing: {s.alarms.join(", ")}</>}
        </p>
        <a className={control.link} href={`#/logs?q=${q}`}>
          Logs for {s.name} →
        </a>
      </div>
      <div className="grid gap-x-6 gap-y-8 md:grid-cols-2">
        <BoardPlot title="Rate" unit="requests / min">
          <MultiPlot series={rate} height={180} unit="req" onPick={pick} ariaLabel={`${s.name} requests per minute, this window${hasLastWeek ? " and last week" : ""}`} />
          <PlotLegend labels={["this window", board.data && !hasLastWeek ? "last week · no data" : "last week"]} />
        </BoardPlot>
        <BoardPlot title="Duration" unit="ms · p50 p90 p95 p99">
          {board.error && !board.data ? (
            <ErrorNote error={board.error} what="duration percentiles" />
          ) : !board.data ? (
            <Skeleton className="h-[180px] w-full" />
          ) : (
            <>
              <MultiPlot series={board.data.percentiles} threshold={threshold} height={180} unit="ms" onPick={pick} ariaLabel={`${s.name} latency percentiles per minute`} />
              <PlotLegend labels={[...PERCENTILES]} threshold={threshold != null ? `alarm at ${num(threshold)} ms` : undefined} />
            </>
          )}
        </BoardPlot>
        <BoardPlot title="Errors" unit="5xx / min">
          <MultiPlot series={[{ label: "5xx", t: grid, v: s.minutes.err }]} height={180} unit="/ min" onPick={pick} ariaLabel={`${s.name} 5xx responses per minute`} />
        </BoardPlot>
        <BoardPlot title="Error rate" unit="% of requests 5xx">
          <MultiPlot series={[{ label: "5xx %", t: grid, v: errPct }]} threshold={DEGRADED_ERROR_PCT} height={180} unit="%" onPick={pick} ariaLabel={`${s.name} share of requests that were 5xx, per minute`} />
          <PlotLegend labels={[]} threshold={`degraded from ${DEGRADED_ERROR_PCT} %`} />
        </BoardPlot>
      </div>
    </div>
  );
}

function BoardPlot({ title, unit, children }: { title: string; unit: string; children: ReactNode }) {
  return (
    <figure className="min-w-0">
      <figcaption className={`${typography.micro} mb-2 flex justify-between gap-3`}>
        <span>{title}</span>
        <span className="text-muted-foreground">{unit}</span>
      </figcaption>
      {children}
    </figure>
  );
}
