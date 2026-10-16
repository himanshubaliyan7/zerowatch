/**
 * Alarm rules with their state, and the last N evaluation periods drawn as
 * squares: filled red = breaching, outlined = within threshold, dashed = no
 * data. The squares are recomputed here from the rule's own metric query, so
 * they show what the evaluator saw.
 */
import { useEffect, useState } from "react";
import { endpoints, type AlarmRule, type AlarmTransition, type MetricStat } from "@/lib/api";
import { subscribeLive } from "@/lib/live";
import { usePoll, useRoute } from "@/lib/hooks";
import { AlarmStudio } from "@/components/AlarmStudio";
import { ALARM_STATE } from "@/lib/status";
import { ago, breaches, comparison, num, plural } from "@/lib/format";
import { Empty, ErrorNote, PageHead, Section, Skeleton, stagger, Verdict } from "@/design/primitives";
import { control, typography } from "@/design/tokens";

const MAX_SQUARES = 10;

const STATE = ALARM_STATE;

export function Alarms() {
  const rules = usePoll(() => endpoints.alarms(), 10_000);
  const { refresh } = rules;
  useEffect(() => subscribeLive((ev) => ev.kind === "alarm" && refresh()), [refresh]);

  /** The open studio: a new rule (optionally prefilled) or the id of the rule being edited. */
  const [studio, setStudio] = useState<{ edit?: string; metric?: MetricStat; forecastSec?: number; key: number } | null>(null);
  const route = useRoute();
  // Deep link "#/alarms?new=<JSON MetricStat>[&forecast=<seconds>]" opens a prefilled new rule once
  // (predictive when forecast is set), then the link is cleared.
  useEffect(() => {
    const raw = route.view === "alarms" ? route.params.get("new") : null;
    if (raw == null) return;
    let metric: MetricStat | undefined;
    try {
      const m = JSON.parse(raw) as Partial<MetricStat>;
      if (m && typeof m.namespace === "string" && typeof m.metricName === "string")
        metric = { namespace: m.namespace, metricName: m.metricName, stat: m.stat || "Average", dimensions: m.dimensions ?? {} };
    } catch {
      // A malformed link still opens the studio, on the default metric.
    }
    const fs = Number(route.params.get("forecast"));
    setStudio({ metric, forecastSec: Number.isFinite(fs) && fs > 0 ? fs : undefined, key: Date.now() });
    window.history.replaceState(null, "", "#/alarms");
  }, [route]);
  const close = () => setStudio(null);
  const done = () => {
    setStudio(null);
    refresh();
  };

  const list = [...(rules.data ?? [])].sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name));
  const firing = list.filter((r) => r.state === "ALARM").length;
  const auto = list.filter((r) => r.description?.startsWith("Auto-calibrated")).length;

  const headline = !rules.data
    ? "Checking…"
    : list.length === 0
      ? "No rules yet."
      : firing === 0
        ? "Nothing is wrong."
        : firing === 1
          ? "One thing is wrong."
          : `${num(firing)} things are wrong.`;

  return (
    <div className="space-y-12 sm:space-y-16">
      <PageHead eyebrow={`${plural(list.length, "rule")} · ${num(auto)} auto-calibrated by the supervisor`} lines={[headline]} />
      <Section
        index={1}
        title="Rules"
        note="squares = last periods, oldest first · filled = breaching · dashed = no data"
        action={
          <button type="button" className={studio && !studio.edit ? control.buttonActive : control.button} onClick={() => setStudio({ key: Date.now() })}>
            + New alarm
          </button>
        }
      >
        {studio && !studio.edit && (
          <div className="mb-6">
            <AlarmStudio key={studio.key} initialMetric={studio.metric} initialForecastSec={studio.forecastSec} onClose={close} onSaved={done} />
          </div>
        )}
        {rules.error && !rules.data ? (
          <ErrorNote error={rules.error} what="alarm rules" />
        ) : !rules.data ? (
          <div className="space-y-3">
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} className="h-24 w-full" />
            ))}
          </div>
        ) : list.length === 0 ? (
          <Empty>
            No alarm rules. The supervisor creates a p99 latency alarm per service once it has a 30-minute baseline; you can also POST one to
            <span className="font-mono text-[0.8rem]"> /api/v1/alarms</span>.
          </Empty>
        ) : (
          <div className="border-b border-hair">
            {list.map((r, i) => (
              <AlarmRow
                key={r.id}
                rule={r}
                index={i}
                editing={studio?.edit === r.id}
                onEdit={() => setStudio({ edit: r.id, key: Date.now() })}
                onClose={close}
                onChanged={done}
              />
            ))}
          </div>
        )}
      </Section>
    </div>
  );
}

function rank(r: AlarmRule): number {
  return r.state === "ALARM" ? 0 : r.state === "OK" ? 2 : 1;
}

type Period = "breach" | "ok" | "missing";

function AlarmRow({
  rule,
  index,
  editing,
  onEdit,
  onClose,
  onChanged,
}: {
  rule: AlarmRule;
  index: number;
  editing: boolean;
  onEdit: () => void;
  onClose: () => void;
  /** After a save or delete: close the studio and refresh the list. */
  onChanged: () => void;
}) {
  const st = STATE[rule.state] ?? STATE[""];
  const n = Math.min(MAX_SQUARES, Math.max(1, rule.config.evaluationPeriods));
  const periods = usePoll((signal) => evaluate(rule, n, signal), 30_000, [rule.id, rule.updatedAt, rule.state]);
  const [last, setLast] = useState<AlarmTransition>();
  useEffect(() => {
    endpoints
      .alarmHistory(rule.id, 1)
      .then((h) => setLast(h[0]))
      .catch(() => setLast(undefined));
  }, [rule.id, rule.state]);

  const m = rule.metric;
  const what = m ? `${m.stat} ${m.metricName}${m.dimensions?.Service ? ` · ${m.dimensions.Service}` : ""}` : (rule.expression ?? "expression");
  const unit = m?.metricName === "Latency" ? " ms" : "";

  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string>();
  const remove = async () => {
    setDeleting(true);
    setDeleteError(undefined);
    try {
      await endpoints.deleteAlarm(rule.id);
      setConfirming(false);
      onChanged();
    } catch (e) {
      setDeleteError(e instanceof Error ? e.message : String(e));
    } finally {
      setDeleting(false);
    }
  };

  return (
    <>
      <article className="rise-in grid gap-x-8 gap-y-3 border-t border-hair py-5 sm:grid-cols-[minmax(0,1fr)_auto]" style={stagger(index + 2)}>
        <div className="min-w-0">
          <h3 className={typography.itemName}>{rule.name}</h3>
          {rule.description && <p className="mt-1.5 max-w-[62ch] text-[0.95rem] leading-relaxed text-muted-foreground">{rule.description}</p>}
          <p className={`${typography.eyebrow} mt-2`}>
            {what} {comparison(rule.config.comparison)} {num(rule.config.threshold)}
            {unit} · {rule.config.datapointsToAlarm} of {rule.config.evaluationPeriods} × {num(rule.periodSec)} s · missing = {rule.config.treatMissingData}
            {rule.forecastHorizonSec ? ` · forecast next ${rule.forecastHorizonSec >= 3600 ? `${num(rule.forecastHorizonSec / 3600, rule.forecastHorizonSec % 3600 ? 1 : 0)} h` : `${num(rule.forecastHorizonSec / 60)} min`}` : ""}
            {!rule.enabled && " · disabled"}
          </p>
          <div className="mt-3 flex gap-[3px]" role="img" aria-label={periodsLabel(periods.data)}>
            {(periods.data ?? new Array<Period>(n).fill("missing")).map((p, i) => (
              <span
                key={i}
                className={`pop inline-block h-3 w-3 border ${p === "breach" ? "border-tone-critical bg-tone-critical" : p === "ok" ? "border-border" : "border-dashed border-border"}`}
                style={stagger(i)}
              />
            ))}
          </div>
        </div>
        <div className="flex flex-col items-start gap-2 sm:items-end">
          <Verdict tone={st.tone} label={st.label} />
          {last && (
            <p className={`${typography.eyebrow} max-w-[30ch] sm:text-right`}>
              {last.from ? `${STATE[last.from]?.label ?? last.from} → ` : ""}
              {STATE[last.to]?.label ?? last.to} · {ago(last.timestamp)}
            </p>
          )}
          {confirming ? (
            <div className={`${typography.micro} flex flex-wrap items-center gap-2 sm:justify-end`} role="alert">
              <span className="max-w-[24ch] truncate normal-case tracking-normal">Delete {rule.name}?</span>
              <button type="button" className={control.buttonActive} onClick={() => void remove()} disabled={deleting}>
                {deleting ? "Deleting…" : "Yes"}
              </button>
              <button type="button" className={control.button} onClick={() => setConfirming(false)} disabled={deleting}>
                Cancel
              </button>
            </div>
          ) : (
            <div className="flex gap-2">
              <button type="button" className={editing ? control.buttonActive : control.button} onClick={editing ? onClose : onEdit} aria-expanded={editing}>
                {editing ? "Close" : "Edit"}
              </button>
              <button type="button" className={control.button} onClick={() => setConfirming(true)}>
                Delete
              </button>
            </div>
          )}
          {deleteError && (
            <p className="max-w-[34ch] text-[0.85rem] sm:text-right">
              <span className="text-tone-critical">Not deleted:</span> <span className="font-mono text-[0.8rem]">{deleteError}</span>
            </p>
          )}
        </div>
      </article>
      {editing && (
        <div className="pb-6">
          <AlarmStudio rule={rule} onClose={onClose} onSaved={onChanged} />
        </div>
      )}
    </>
  );
}

function periodsLabel(p?: Period[]): string {
  if (!p) return "Evaluation periods loading";
  const b = p.filter((x) => x === "breach").length;
  const m = p.filter((x) => x === "missing").length;
  return `Last ${p.length} periods: ${b} breaching, ${p.length - b - m} within threshold, ${m} without data`;
}

async function evaluate(rule: AlarmRule, n: number, signal: AbortSignal): Promise<Period[]> {
  const p = Math.max(1, rule.periodSec) * 1000;
  const end = Math.floor(Date.now() / p) * p;
  const start = end - n * p;
  const queries = rule.metric
    ? rule.expression
      ? [
          { id: "m1", metricStat: rule.metric, returnData: false },
          { id: "e1", expression: rule.expression },
        ]
      : [{ id: "m1", metricStat: rule.metric }]
    : [];
  if (!queries.length) return new Array<Period>(n).fill("missing");
  const res = await endpoints.query({ queries, start, end, period: rule.periodSec }, signal);
  const r = res.find((x) => x.id === (rule.expression ? "e1" : "m1"));
  const byT = new Map<number, number>();
  r?.timestamps.forEach((t, i) => byT.set(Math.floor(t / p) * p, r.values[i]));
  return Array.from({ length: n }, (_, i) => {
    const v = byT.get(start + i * p);
    if (v == null || !Number.isFinite(v)) return "missing";
    return breaches(v, rule.config.comparison, rule.config.threshold) ? "breach" : "ok";
  });
}
