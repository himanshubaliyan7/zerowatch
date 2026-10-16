/**
 * Choose a stored metric: namespace, metric name, a value (or "*" = any) for
 * every dimension the series carry, and a statistic. Storage matches
 * dimensions exactly, so the picker always names every key.
 */
import { useMemo } from "react";
import { endpoints, type MetricStat, type SeriesMeta } from "@/lib/api";
import { usePoll } from "@/lib/hooks";
import { control, typography } from "@/design/tokens";

export const STATS = ["Average", "Sum", "Minimum", "Maximum", "SampleCount", "p50", "p90", "p95", "p99"] as const;

function Select({ label, value, options, onChange }: { label: string; value: string; options: { value: string; label: string }[]; onChange: (v: string) => void }) {
  return (
    <label className="block min-w-0">
      <span className={`${typography.eyebrow} mb-1 block`}>{label}</span>
      <span className="relative block">
        <select className={control.select} value={value} onChange={(e) => onChange(e.target.value)}>
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[0.6rem]" aria-hidden="true">
          ▼
        </span>
      </span>
    </label>
  );
}

/** All stored series, refreshed every minute; shared shape for pickers. */
export function useSeriesCatalog() {
  return usePoll(() => endpoints.series("", "", 10_000), 60_000);
}

export function MetricPicker({ value, onChange, catalog }: { value: MetricStat; onChange: (m: MetricStat) => void; catalog: SeriesMeta[] }) {
  const namespaces = useMemo(() => [...new Set(catalog.map((s) => s.namespace))].sort(), [catalog]);
  const names = useMemo(() => [...new Set(catalog.filter((s) => s.namespace === value.namespace).map((s) => s.metricName))].sort(), [catalog, value.namespace]);
  const matching = useMemo(() => catalog.filter((s) => s.namespace === value.namespace && s.metricName === value.metricName), [catalog, value.namespace, value.metricName]);
  const keys = useMemo(() => [...new Set(matching.flatMap((s) => Object.keys(s.dimensions ?? {})))].sort(), [matching]);

  const set = (patch: Partial<MetricStat>) => onChange({ ...value, ...patch });
  const pickMetric = (namespace: string, metricName: string) => {
    const first = catalog.find((s) => s.namespace === namespace && s.metricName === metricName);
    const dims = Object.fromEntries(Object.keys(first?.dimensions ?? {}).map((k) => [k, "*"]));
    onChange({ ...value, namespace, metricName, dimensions: dims });
  };

  if (!catalog.length) {
    return <p className="text-[0.95rem] text-muted-foreground">No metrics stored yet.</p>;
  }

  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
      <Select
        label="Namespace"
        value={value.namespace}
        options={namespaces.map((n) => ({ value: n, label: n }))}
        onChange={(ns) => {
          const first = catalog.find((s) => s.namespace === ns);
          pickMetric(ns, first?.metricName ?? "");
        }}
      />
      <Select label="Metric" value={value.metricName} options={names.map((n) => ({ value: n, label: n }))} onChange={(m) => pickMetric(value.namespace, m)} />
      {keys.map((k) => {
        const vals = [...new Set(matching.map((s) => s.dimensions?.[k]).filter((v): v is string => !!v))].sort();
        return (
          <Select
            key={k}
            label={k}
            value={value.dimensions?.[k] ?? "*"}
            options={[{ value: "*", label: `any ${k}` }, ...vals.map((v) => ({ value: v, label: v }))]}
            onChange={(v) => set({ dimensions: { ...(value.dimensions ?? {}), [k]: v } })}
          />
        );
      })}
      <Select label="Statistic" value={value.stat} options={STATS.map((s) => ({ value: s, label: s }))} onChange={(stat) => set({ stat })} />
    </div>
  );
}

/** A reasonable first pick: RED p99 latency across everything, else the first series. */
export function defaultMetric(catalog: SeriesMeta[]): MetricStat {
  const red = catalog.find((s) => s.namespace === "ZeroWatch/RED" && s.metricName === "Latency") ?? catalog[0];
  if (!red) return { namespace: "", metricName: "", stat: "Average", dimensions: {} };
  return {
    namespace: red.namespace,
    metricName: red.metricName,
    stat: red.metricName === "Latency" ? "p99" : "Average",
    dimensions: Object.fromEntries(Object.keys(red.dimensions ?? {}).map((k) => [k, "*"])),
  };
}
