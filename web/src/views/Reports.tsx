/**
 * Reports: the periodic summary (GET /api/v1/reports/summary) for the last
 * 24 h, 7 d or 30 d. A narrative lead, the totals, the runway of capacity
 * forecasts (when each alarm threshold is forecast to be crossed), the
 * per-service table with p99 change on the previous period, alarm activity,
 * anomalies and the slowest traces. The same report is available as a
 * self-contained HTML page and can be sent to the configured targets.
 */
import { useState } from "react";
import { endpoints, withKey, type AlarmState, type Report } from "@/lib/api";
import { usePoll } from "@/lib/hooks";
import { clock, num, plural } from "@/lib/format";
import { Empty, ErrorNote, PageHead, Section, Skeleton, StatusMark, stagger } from "@/design/primitives";
import { control, layout, typography, type Tone } from "@/design/tokens";
import { Runway } from "@/components/reports/Runway";

const RANGES = [
  { id: "24h", label: "24h", words: "24 hours.", ms: 24 * 3600_000 },
  { id: "7d", label: "7d", words: "7 days.", ms: 7 * 24 * 3600_000 },
  { id: "30d", label: "30d", words: "30 days.", ms: 30 * 24 * 3600_000 },
] as const;

const STATE_TONE: Record<AlarmState | "", Tone> = { ALARM: "critical", OK: "positive", INSUFFICIENT_DATA: "unknown", "": "unknown" };

function compact(v: number): string {
  if (v >= 1e9) return `${num(v / 1e9, 1)}B`;
  if (v >= 1e6) return `${num(v / 1e6, 1)}M`;
  if (v >= 1e4) return `${num(v / 1e3, 1)}k`;
  return num(v);
}

const pct = (v: number | null, d = 2) => (v == null ? "—" : `${num(v, d)}%`);
const msv = (v: number | null) => (v == null ? "—" : `${num(v, v < 10 ? 1 : 0)} ms`);
const errTone = (v: number | null) => (v == null ? "" : v >= 5 ? "text-tone-critical" : v >= 1 ? "text-tone-warning" : "");
const changeTone = (v: number | null) => (v == null ? "" : v > 20 ? "text-tone-critical" : v < -20 ? "text-tone-positive" : "");

function stamp(t: number, spanMs: number): string {
  const d = new Date(t);
  const time = d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
  return spanMs >= 24 * 3600_000 || d.toDateString() !== new Date().toDateString()
    ? `${d.toLocaleDateString("en-GB", { day: "numeric", month: "short" })} ${time}`
    : time;
}

export function Reports({ range }: { range?: string }) {
  const rg = RANGES.find((r) => r.id === range) ?? RANGES[0];
  const rep = usePoll((signal) => endpoints.report(rg.id, signal), 60_000, [rg.id]);
  const [send, setSend] = useState<{ busy?: boolean; sent?: string[]; error?: string }>({});

  const sendNow = async () => {
    setSend({ busy: true });
    try {
      const r = await endpoints.sendReport(rg.id);
      setSend({ sent: r.sent });
    } catch (e) {
      setSend({ error: e instanceof Error ? e.message : String(e) });
    }
  };

  const r = rep.data;
  const eyebrow = r ? `Report · ${stamp(r.range.start, rg.ms)} → ${stamp(r.range.end, rg.ms)}` : "Report";

  return (
    <div className={layout.stack}>
      <div className="space-y-6">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-wrap gap-1.5" role="group" aria-label="Report range">
            {RANGES.map((x) => (
              <a key={x.id} href={`#/reports?range=${x.id}`} className={x.id === rg.id ? control.buttonActive : control.button} aria-current={x.id === rg.id ? "page" : undefined}>
                {x.label}
              </a>
            ))}
          </div>
          <div className="flex flex-wrap gap-1.5">
            <a className={control.button} href={withKey(`/api/v1/reports/summary.html?range=${rg.id}`)} target="_blank" rel="noreferrer">
              Open HTML ↗
            </a>
            <button type="button" className={control.button} onClick={() => void sendNow()} disabled={send.busy}>
              {send.busy ? "Sending…" : "Send now"}
            </button>
          </div>
        </div>
        {(send.sent || send.error) && (
          <p className="text-[0.95rem]" aria-live="polite">
            {send.error ? (
              <>
                <span className="text-tone-critical">Not sent:</span> <span className="font-mono text-[0.8rem]">{send.error}</span>
              </>
            ) : (
              <>Sent to {send.sent!.join(", ")}.</>
            )}
          </p>
        )}
        <PageHead mega eyebrow={eyebrow} lines={["Last", rg.words]} lead={r?.narrative} />
      </div>

      {rep.error && !r ? (
        <ErrorNote error={rep.error} what="the report" />
      ) : !r ? (
        <div className="space-y-4">
          <Skeleton className="h-32 w-full" />
          <Skeleton className="h-48 w-full" />
        </div>
      ) : (
        <ReportBody r={r} spanMs={rg.ms} rangeLabel={rg.id} />
      )}
    </div>
  );
}

function Cell({ label, value, unit, tone, i }: { label: string; value: string; unit: string; tone?: string; i: number }) {
  return (
    <div className={`${layout.stat} rise-in`} style={stagger(i)}>
      <div className={`${typography.micro} muted text-muted-foreground`}>{label}</div>
      {/* five across on desktop: a step smaller than typography.stat, so "418 ms" stays on one line */}
      <div className={`my-2 whitespace-nowrap font-display text-[clamp(2rem,3.4vw,3.6rem)] font-semibold leading-[0.95] tracking-[-0.02em] tabular-nums ${tone ?? ""}`}>
        {value}
      </div>
      <div className={`${typography.micro} muted text-muted-foreground`}>{unit}</div>
    </div>
  );
}

function ReportBody({ r, spanMs, rangeLabel }: { r: Report; spanMs: number; rangeLabel: string }) {
  const maxMin = Math.max(1, ...r.alarms.map((a) => a.minutesInAlarm));
  return (
    <>
      <div className="grid grid-cols-2 border-l border-t border-border lg:grid-cols-5">
        <Cell i={1} label="Requests" value={compact(r.totals.requests)} unit={`in ${rangeLabel}`} />
        <Cell i={2} label="Error rate" value={pct(r.totals.errorPct)} unit="of requests" tone={errTone(r.totals.errorPct)} />
        <Cell i={3} label="p50" value={msv(r.totals.p50)} unit="request-weighted" />
        <Cell i={4} label="p99" value={msv(r.totals.p99)} unit="request-weighted" />
        <Cell i={5} label="Good periods" value={pct(r.totals.availabilityPct, 1)} unit="error rate under 5 %" />
      </div>

      <Section index={2} title="Runway" note={`when each alarm threshold is forecast to be crossed · next ${rangeLabel}`}>
        {r.capacity.length ? (
          <Runway rows={r.capacity} start={r.range.end} spanMs={spanMs} label={rangeLabel} />
        ) : (
          <Empty hatch>No forecasts: there are no enabled alarm rules, or not enough history yet.</Empty>
        )}
        {r.capacity.length > 0 && (
          <p className={`${typography.eyebrow} mt-2`}>methods: {[...new Set(r.capacity.map((c) => c.method))].join(", ")}</p>
        )}
      </Section>

      <div className="grid gap-12 xl:grid-cols-2">
        <Section index={3} title="Services" note={`p99 change vs the previous ${rangeLabel}`}>
          {r.services.length ? (
            <div className="relative overflow-x-auto">
              <table className="w-full min-w-[30rem] border-collapse text-left">
                <thead>
                  <tr className={typography.eyebrow}>
                    <th className="pb-2.5 pr-3 font-medium">Service</th>
                    <th className="pb-2.5 pr-3 text-right font-medium">Requests</th>
                    <th className="pb-2.5 pr-3 text-right font-medium">Errors</th>
                    <th className="pb-2.5 pr-3 text-right font-medium">p99</th>
                    <th className="pb-2.5 pr-3 text-right font-medium">Change</th>
                    <th className="pb-2.5 text-right font-medium">Good</th>
                  </tr>
                </thead>
                <tbody className={typography.number}>
                  {r.services.map((s) => (
                    <tr key={s.name} className="border-t border-hair">
                      <td className="py-2.5 pr-3">
                        <a className="border-b border-foreground/40 hover:border-foreground" href={`#/traces?service=${encodeURIComponent(s.name)}`}>
                          {s.name}
                        </a>
                      </td>
                      <td className="py-2.5 pr-3 text-right">{compact(s.requests)}</td>
                      <td className={`py-2.5 pr-3 text-right ${errTone(s.errorPct)}`}>{pct(s.errorPct)}</td>
                      <td className="py-2.5 pr-3 text-right">{msv(s.p99)}</td>
                      <td className={`py-2.5 pr-3 text-right ${changeTone(s.p99ChangePct)}`}>
                        {s.p99ChangePct == null ? "—" : `${s.p99ChangePct > 0 ? "▲ +" : s.p99ChangePct < 0 ? "▼ " : ""}${num(s.p99ChangePct, 0)}%`}
                      </td>
                      <td className="py-2.5 text-right">{pct(s.availabilityPct, 1)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <Empty hatch>No service reported RED metrics in this window.</Empty>
          )}
        </Section>

        <Section index={4} title="Alarms" note="times fired · minutes in alarm">
          {r.alarms.length ? (
            <ul className="border-b border-hair">
              {r.alarms.map((a, i) => (
                <li key={a.id} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-4 gap-y-1 border-t border-hair py-2.5">
                  <a className="min-w-0 truncate hover:underline" href="#/alarms" title={a.name}>
                    {a.name}
                  </a>
                  <StatusMark tone={STATE_TONE[a.state] ?? "unknown"} label={a.state ? a.state.replace("_", " ") : "UNKNOWN"} />
                  <span className="flex min-w-0 items-center gap-3">
                    <span
                      className="grow-x inline-block h-1.5 bg-foreground"
                      style={stagger(i, { width: `${Math.max(a.minutesInAlarm > 0 ? 2 : 0, (a.minutesInAlarm / maxMin) * 70)}%` })}
                      aria-hidden="true"
                    />
                    <span className={`${typography.micro} text-muted-foreground`}>{num(a.minutesInAlarm, a.minutesInAlarm < 10 ? 1 : 0)} min</span>
                  </span>
                  <span className={`${typography.micro} text-muted-foreground`}>fired {a.fired}×</span>
                </li>
              ))}
            </ul>
          ) : (
            <Empty>No alarm rules.</Empty>
          )}
        </Section>
      </div>

      <div className="grid gap-12 xl:grid-cols-2">
        <Section index={5} title="Anomalies" note="outside the robust STL band">
          {r.anomalies.length ? (
            <div className="relative overflow-x-auto">
              <table className="w-full min-w-[30rem] border-collapse text-left">
                <thead>
                  <tr className={typography.eyebrow}>
                    <th className="pb-2.5 pr-3 font-medium">When</th>
                    <th className="pb-2.5 pr-3 font-medium">Service</th>
                    <th className="pb-2.5 pr-3 font-medium">Metric</th>
                    <th className="pb-2.5 pr-3 text-right font-medium">Value</th>
                    <th className="pb-2.5 text-right font-medium">Expected</th>
                  </tr>
                </thead>
                <tbody className={typography.number}>
                  {r.anomalies.map((a, i) => (
                    <tr key={i} className="border-t border-hair">
                      <td className="py-2.5 pr-3">{stamp(a.at, spanMs)}</td>
                      <td className="py-2.5 pr-3">{a.service}</td>
                      <td className="py-2.5 pr-3">{a.metric}</td>
                      <td className="py-2.5 pr-3 text-right text-tone-critical">{num(a.value, a.value < 10 ? 2 : 0)}</td>
                      <td className="py-2.5 text-right text-muted-foreground">
                        {num(a.expectedLower, 0)} – {num(a.expectedUpper, 0)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <Empty hatch>No anomalies, or not enough history to judge: two seasons are needed.</Empty>
          )}
        </Section>

        <Section index={6} title="Slowest traces" note="open in Traces">
          {r.slowTraces.length ? (
            <div className="relative overflow-x-auto">
              <table className="w-full min-w-[30rem] border-collapse text-left">
                <thead>
                  <tr className={typography.eyebrow}>
                    <th className="pb-2.5 pr-3 font-medium">Started</th>
                    <th className="pb-2.5 pr-3 font-medium">Root</th>
                    <th className="pb-2.5 pr-3 text-right font-medium">Duration</th>
                    <th className="pb-2.5 text-right font-medium">Spans</th>
                  </tr>
                </thead>
                <tbody className={typography.number}>
                  {r.slowTraces.map((t) => (
                    <tr key={t.traceId} className="border-t border-hair">
                      <td className="py-2.5 pr-3">{clock(t.start)}</td>
                      <td className="max-w-[16rem] truncate py-2.5 pr-3">
                        <a className="border-b border-foreground/40 hover:border-foreground" href={`#/traces?id=${t.traceId}`}>
                          {t.rootService} · {t.rootName}
                        </a>
                      </td>
                      <td className="py-2.5 pr-3 text-right">{num(t.durationMs, 0)} ms</td>
                      <td className={`py-2.5 text-right ${t.errors ? "text-tone-critical" : ""}`}>
                        {t.spans}
                        {t.errors ? ` · ${plural(t.errors, "err")}` : ""}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <Empty hatch>No traces in this window.</Empty>
          )}
        </Section>
      </div>

      <p className={typography.eyebrow}>
        Generated {clock(r.generatedAt)}. p50 and p99 are request-weighted means of per-period percentiles; a good period has an error rate below 5 %.
      </p>
    </>
  );
}
