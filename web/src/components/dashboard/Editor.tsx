/**
 * The board builder: board settings, a layout map of the 12-column grid, and
 * one editor per widget (title, type, position, size, and its query). No
 * drag-and-drop: positions are chosen from lists, which also works on phones.
 */
import { useRef, useState, type ReactNode } from "react";
import type { Dashboard, MetricDataQuery, SeriesMeta, Widget } from "@/lib/api";
import { RANGES } from "@/lib/range";
import { control, typography } from "@/design/tokens";
import { stagger } from "@/design/primitives";
import { MetricPicker, defaultMetric } from "@/components/MetricPicker";
import {
  anomalyMethod,
  FORECAST_HORIZONS,
  GRID_COLUMNS,
  forecastOf,
  MAX_H,
  MAX_Y,
  MIN_REFRESH,
  PERIODS,
  WIDGET_TYPES,
  alarmService,
  boardRange,
  boardRefresh,
  isMetricType,
  logQuery,
  metricQuery,
  nextQueryId,
  overlaps,
  textOf,
} from "./model";

const REFRESHES = [10, 30, 60, 120, 300];
const LEVELS = [
  { value: "", label: "any level" },
  { value: "error", label: "error" },
  { value: "warn", label: "warn" },
  { value: "info", label: "info" },
];

const range = (from: number, to: number) => Array.from({ length: to - from + 1 }, (_, i) => from + i);

export function Field({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <label className={`block min-w-0 ${className ?? ""}`}>
      <span className={`${typography.eyebrow} mb-1 block`}>{label}</span>
      {children}
    </label>
  );
}

export function Select({
  label,
  value,
  options,
  onChange,
  className,
}: {
  label: string;
  value: string;
  options: { value: string; label: string }[];
  onChange: (v: string) => void;
  className?: string;
}) {
  return (
    <Field label={label} className={className}>
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
    </Field>
  );
}

const nums = (xs: number[], fmt: (n: number) => string = String) => xs.map((n) => ({ value: String(n), label: fmt(n) }));

// ---- board ----

export function BoardSettings({ draft, onChange }: { draft: Dashboard; onChange: (d: Dashboard) => void }) {
  const layout = draft.layout ?? {};
  const refresh = boardRefresh(draft);
  const refreshes = REFRESHES.includes(refresh) ? REFRESHES : [...REFRESHES, refresh].sort((a, b) => a - b);
  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)_minmax(0,1fr)_minmax(0,1fr)]">
      <Field label="Title (required)">
        <input className={`${control.input} w-full`} value={draft.title} maxLength={200} onChange={(e) => onChange({ ...draft, title: e.target.value })} />
      </Field>
      <Field label="Description">
        <input className={`${control.input} w-full`} value={draft.description ?? ""} onChange={(e) => onChange({ ...draft, description: e.target.value })} />
      </Field>
      <Select
        label="Default range"
        value={boardRange(draft)}
        options={RANGES.map((r) => ({ value: r.id, label: r.label }))}
        onChange={(v) => onChange({ ...draft, layout: { ...layout, range: v } })}
      />
      <Select
        label="Refresh"
        value={String(refresh)}
        options={nums(refreshes.filter((r) => r >= MIN_REFRESH), (s) => (s < 60 ? `every ${s} s` : `every ${s / 60} min`))}
        onChange={(v) => onChange({ ...draft, layout: { ...layout, refresh: Number(v) } })}
      />
    </div>
  );
}

type Pos = Pick<Widget, "x" | "y" | "w" | "h">;

/**
 * Where each widget sits on the 12 columns, numbered as in the list below.
 * With onMove it is also the layout editor: drag a block to move it, drag its
 * corner square to resize, or focus a block and use the arrow keys (move) and
 * Shift+arrows (resize). Positions snap to grid cells and stay on the grid.
 */
export function LayoutMap({ widgets, onMove }: { widgets: Widget[]; onMove?: (index: number, pos: Pos) => void }) {
  const clash = overlaps(widgets);
  const grid = useRef<HTMLDivElement>(null);
  const [drag, setDrag] = useState<{ i: number; mode: "move" | "resize"; px: number; py: number; start: Pos } | null>(null);
  const rows = Math.min(MAX_Y + MAX_H, Math.max(1, ...widgets.map((w) => w.y + w.h)) + (onMove ? 2 : 0));
  if (!widgets.length) return null;
  const clamp = (p: Pos): Pos => {
    const w = Math.max(1, Math.min(GRID_COLUMNS, p.w));
    const h = Math.max(1, Math.min(MAX_H, p.h));
    return { w, h, x: Math.max(0, Math.min(GRID_COLUMNS - w, p.x)), y: Math.max(0, Math.min(MAX_Y, p.y)) };
  };
  const cell = () => {
    const r = grid.current?.getBoundingClientRect();
    return r ? { cw: r.width / GRID_COLUMNS, ch: r.height / rows } : { cw: 1, ch: 1 };
  };
  const onPointerMove = (e: React.PointerEvent) => {
    if (!drag || !onMove) return;
    const { cw, ch } = cell();
    const dx = Math.round((e.clientX - drag.px) / cw);
    const dy = Math.round((e.clientY - drag.py) / ch);
    const s0 = drag.start;
    const next = drag.mode === "move" ? { ...s0, x: s0.x + dx, y: s0.y + dy } : { ...s0, w: Math.min(GRID_COLUMNS - s0.x, s0.w + dx), h: s0.h + dy };
    const c = clamp(next);
    const cur = widgets[drag.i];
    if (c.x !== cur.x || c.y !== cur.y || c.w !== cur.w || c.h !== cur.h) onMove(drag.i, c);
  };
  const onKey = (i: number, e: React.KeyboardEvent) => {
    if (!onMove) return;
    const d = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1] }[e.key];
    if (!d) return;
    e.preventDefault();
    const w = widgets[i];
    onMove(i, clamp(e.shiftKey ? { ...w, w: Math.min(GRID_COLUMNS - w.x, w.w + d[0]), h: w.h + d[1] } : { ...w, x: w.x + d[0], y: w.y + d[1] }));
  };
  return (
    <div>
      <div
        ref={grid}
        className={`plot-grid relative grid touch-none select-none border border-border ${drag ? "cursor-grabbing" : ""}`}
        style={{ gridTemplateColumns: `repeat(${GRID_COLUMNS}, minmax(0, 1fr))`, gridTemplateRows: `repeat(${rows}, ${onMove ? "1.6rem" : "0.9rem"})` }}
        role={onMove ? "group" : "img"}
        aria-label={`Layout: ${widgets.length} widgets on a ${GRID_COLUMNS}-column grid${onMove ? "; arrow keys move the focused widget, Shift+arrows resize it" : ""}`}
        onPointerMove={onPointerMove}
        onPointerUp={() => setDrag(null)}
        onPointerCancel={() => setDrag(null)}
      >
        {widgets.map((w, i) => (
          <div
            key={i}
            tabIndex={onMove ? 0 : undefined}
            className={`${typography.micro} relative flex min-w-0 items-start overflow-hidden border px-1 leading-tight ${
              drag?.i === i ? "z-10 border-foreground bg-foreground text-background" : "border-foreground/60 bg-background/80"
            } ${onMove ? "cursor-grab hover:z-10 focus-visible:z-10 focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-foreground" : ""}`}
            style={{ gridColumn: `${Math.min(w.x, GRID_COLUMNS - 1) + 1} / span ${Math.max(1, Math.min(w.w, GRID_COLUMNS - w.x))}`, gridRow: `${w.y + 1} / span ${Math.max(1, w.h)}` }}
            title={onMove ? `${w.title} · drag to move, corner to resize` : w.title}
            aria-label={onMove ? `${i + 1}: ${w.title || "untitled"}, column ${w.x + 1}, row ${w.y + 1}, ${w.w} wide, ${w.h} high` : undefined}
            onKeyDown={(e) => onKey(i, e)}
            onPointerDown={
              onMove
                ? (e) => {
                    e.currentTarget.parentElement?.setPointerCapture(e.pointerId);
                    setDrag({ i, mode: "move", px: e.clientX, py: e.clientY, start: { x: w.x, y: w.y, w: w.w, h: w.h } });
                  }
                : undefined
            }
          >
            {i + 1}
            {onMove && (
              <span
                className={`absolute bottom-0 right-0 h-2.5 w-2.5 cursor-nwse-resize ${drag?.i === i ? "bg-background" : "bg-foreground"}`}
                aria-hidden="true"
                onPointerDown={(e) => {
                  e.stopPropagation();
                  e.currentTarget.parentElement?.parentElement?.setPointerCapture(e.pointerId);
                  setDrag({ i, mode: "resize", px: e.clientX, py: e.clientY, start: { x: w.x, y: w.y, w: w.w, h: w.h } });
                }}
              />
            )}
          </div>
        ))}
      </div>
      {clash.length > 0 && (
        <p className={`${typography.eyebrow} mt-2`}>
          overlapping: {clash.map(([a, b]) => `${a + 1} and ${b + 1}`).join(", ")} · they will stack on top of each other
        </p>
      )}
    </div>
  );
}

// ---- widget ----

function defaultQuery(type: Widget["type"], current: Widget, catalog: SeriesMeta[]): unknown {
  if (isMetricType(type)) {
    const mq = metricQuery(current);
    const extra = type === "forecast" ? { horizonMin: forecastOf(current).horizonMin } : {};
    if (mq.queries.length) return { queries: mq.queries, period: mq.period ?? 60, ...extra };
    return { queries: [{ id: "m1", metricStat: defaultMetric(catalog) }], period: 60, ...extra };
  }
  if (type === "logs") return { level: "error" };
  if (type === "text") return { text: "" };
  return {};
}

export function newWidget(widgets: Widget[], catalog: SeriesMeta[]): Widget {
  const y = Math.min(MAX_Y, widgets.reduce((m, w) => Math.max(m, w.y + w.h), 0));
  return { type: "timeseries", title: "New chart", query: { queries: [{ id: "m1", metricStat: defaultMetric(catalog) }], period: 60 }, x: 0, y, w: 6, h: 3 };
}

export function WidgetEditor({
  widget,
  index,
  count,
  catalog,
  services,
  onChange,
  onRemove,
  onMove,
}: {
  widget: Widget;
  index: number;
  count: number;
  catalog: SeriesMeta[];
  services: string[];
  onChange: (w: Widget) => void;
  onRemove: () => void;
  onMove: (dir: -1 | 1) => void;
}) {
  const set = (patch: Partial<Widget>) => onChange({ ...widget, ...patch });
  return (
    <article className="rise-in border-t border-border pt-4" style={stagger(index)} aria-label={`Widget ${index + 1}`}>
      <div className="mb-3 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2">
        <h3 className={typography.micro}>
          Widget {index + 1} <span className="text-muted-foreground">of {count}</span>
        </h3>
        <div className="flex flex-wrap gap-2">
          <button type="button" className={control.button} disabled={index === 0} onClick={() => onMove(-1)} aria-label="Move up in the list">
            ↑
          </button>
          <button type="button" className={control.button} disabled={index === count - 1} onClick={() => onMove(1)} aria-label="Move down in the list">
            ↓
          </button>
          <button type="button" className={control.button} onClick={onRemove}>
            Remove
          </button>
        </div>
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)_repeat(4,minmax(0,1fr))]">
        <Field label="Title">
          <input className={`${control.input} w-full`} value={widget.title} onChange={(e) => set({ title: e.target.value })} />
        </Field>
        <Select
          label="Type"
          value={widget.type}
          options={WIDGET_TYPES.map((t) => ({ value: t.id, label: t.label }))}
          onChange={(v) => {
            const type = v as Widget["type"];
            set({ type, query: defaultQuery(type, widget, catalog) });
          }}
        />
        <Select
          label="Column (x)"
          value={String(widget.x)}
          options={nums(range(0, GRID_COLUMNS - 1))}
          onChange={(v) => {
            const x = Number(v);
            set({ x, w: Math.min(widget.w, GRID_COLUMNS - x) });
          }}
        />
        <Select label="Width (w)" value={String(widget.w)} options={nums(range(1, GRID_COLUMNS - widget.x))} onChange={(v) => set({ w: Number(v) })} />
        <Select label="Row (y)" value={String(widget.y)} options={nums(range(0, Math.max(MAX_Y, widget.y)))} onChange={(v) => set({ y: Number(v) })} />
        <Select label="Height (h)" value={String(widget.h)} options={nums(range(1, Math.max(MAX_H, widget.h)))} onChange={(v) => set({ h: Number(v) })} />
      </div>
      <div className="mt-4">
        <QueryEditor widget={widget} catalog={catalog} services={services} onChange={(query) => set({ query })} />
      </div>
    </article>
  );
}

function QueryEditor({ widget, catalog, services, onChange }: { widget: Widget; catalog: SeriesMeta[]; services: string[]; onChange: (q: unknown) => void }) {
  if (isMetricType(widget.type)) return <MetricQueriesEditor widget={widget} catalog={catalog} onChange={onChange} />;
  if (widget.type === "logs") {
    const q = logQuery(widget);
    return (
      <div className="grid gap-3 sm:grid-cols-[minmax(0,3fr)_minmax(0,1fr)]">
        <Field label="Filter · terms must all match, key=value matches attributes, -term excludes">
          <input
            className={`${control.input} w-full`}
            value={q.query ?? ""}
            placeholder="service=checkout timeout"
            onChange={(e) => onChange({ ...q, query: e.target.value || undefined })}
          />
        </Field>
        <Select label="Level" value={q.level ?? ""} options={LEVELS} onChange={(v) => onChange({ ...q, level: v || undefined })} />
      </div>
    );
  }
  if (widget.type === "text") {
    return (
      <Field label="Text">
        <textarea className={`${control.input} block min-h-28 w-full resize-y font-sans text-[0.95rem]`} value={textOf(widget)} onChange={(e) => onChange({ text: e.target.value })} />
      </Field>
    );
  }
  if (widget.type === "alarm") {
    const svc = alarmService(widget) ?? "";
    const opts = services.includes(svc) || !svc ? services : [svc, ...services];
    return (
      <Select
        className="sm:max-w-sm"
        label="Service"
        value={svc}
        options={[{ value: "", label: "every service" }, ...opts.map((s) => ({ value: s, label: s }))]}
        onChange={(v) => onChange(v ? { service: v } : {})}
      />
    );
  }
  return null;
}

function MetricQueriesEditor({ widget, catalog, onChange }: { widget: Widget; catalog: SeriesMeta[]; onChange: (q: unknown) => void }) {
  const { queries, period } = metricQuery(widget);
  const fc = forecastOf(widget);
  const forecastPart = (f = fc) =>
    widget.type === "forecast"
      ? { horizonMin: f.horizonMin, ...(f.threshold != null ? { threshold: f.threshold } : {}) }
      : widget.type === "anomaly"
        ? { method: anomalyMethod(widget) }
        : {};
  const emit = (qs: MetricDataQuery[], p = period) => onChange({ queries: qs, period: p ?? 60, ...forecastPart() });
  const setAt = (i: number, q: MetricDataQuery) => emit(queries.map((x, k) => (k === i ? q : x)));
  const note =
    widget.type === "stat"
      ? "the stat shows the latest value of the first shown series"
      : widget.type === "anomaly"
        ? "the band is fitted to the first shown series"
        : widget.type === "forecast"
          ? "the first shown series is forecast from its stored history"
          : "every shown series is drawn";
  return (
    <div>
      {widget.type === "anomaly" && (
        <div className="mb-3">
          <Select
            className="w-64"
            label="Band model"
            value={anomalyMethod(widget)}
            options={[
              { value: "holtwinters", label: "Holt-Winters (online)" },
              { value: "stl", label: "STL (robust, needs 2 seasons)" },
            ]}
            onChange={(v) => onChange({ queries, period: period ?? 60, method: v })}
          />
        </div>
      )}
      {widget.type === "forecast" && (
        <div className="mb-3 flex flex-wrap items-end gap-3">
          <Select
            className="w-48"
            label="Forecast"
            value={String(fc.horizonMin)}
            options={nums(
              FORECAST_HORIZONS.includes(fc.horizonMin as (typeof FORECAST_HORIZONS)[number]) ? [...FORECAST_HORIZONS] : [...FORECAST_HORIZONS, fc.horizonMin].sort((a, b) => a - b),
              (m) => (m < 60 ? `next ${m} min` : `next ${m / 60} h`),
            )}
            onChange={(v) => onChange({ queries, period: period ?? 60, ...forecastPart({ ...fc, horizonMin: Number(v) }) })}
          />
          <label className="block w-40">
            <span className={`${typography.eyebrow} mb-1 block`}>Threshold</span>
            <input
              className={`${control.input} w-full tabular-nums`}
              inputMode="decimal"
              placeholder="optional"
              defaultValue={fc.threshold ?? ""}
              onChange={(e) => {
                const n = e.target.value.trim() === "" ? undefined : Number(e.target.value);
                onChange({ queries, period: period ?? 60, ...forecastPart({ ...fc, threshold: n != null && Number.isFinite(n) ? n : undefined }) });
              }}
            />
          </label>
        </div>
      )}
      <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
        <Select
          className="w-48"
          label="Period"
          value={String(period ?? 60)}
          options={nums(
            PERIODS.includes((period ?? 60) as (typeof PERIODS)[number]) ? [...PERIODS] : [...PERIODS, period ?? 60].sort((a, b) => a - b),
            (s) => (s < 3600 ? `${s / 60} min` : `${s / 3600} h`),
          )}
          onChange={(v) => emit(queries, Number(v))}
        />
        <p className={`${typography.eyebrow} max-w-[48ch]`}>{note} · the range's own period is used when coarser</p>
      </div>
      <ol className="space-y-3">
        {queries.map((q, i) => (
          <li key={i} className="border border-hair p-3">
            <QueryRow
              query={q}
              catalog={catalog}
              onChange={(nq) => setAt(i, nq)}
              onRemove={queries.length > 1 ? () => emit(queries.filter((_, k) => k !== i)) : undefined}
            />
          </li>
        ))}
      </ol>
      <div className="mt-3 flex flex-wrap gap-2">
        <button type="button" className={control.button} onClick={() => emit([...queries, { id: nextQueryId(queries), metricStat: defaultMetric(catalog) }])}>
          Add metric
        </button>
        <button
          type="button"
          className={control.button}
          onClick={() => emit([...queries, { id: nextQueryId(queries, "e"), label: "expression", expression: queries[0] ? `${queries[0].id} * 1` : "" }])}
        >
          Add expression
        </button>
      </div>
    </div>
  );
}

function QueryRow({ query, catalog, onChange, onRemove }: { query: MetricDataQuery; catalog: SeriesMeta[]; onChange: (q: MetricDataQuery) => void; onRemove?: () => void }) {
  const isExpr = query.expression != null && !query.metricStat;
  const shown = query.returnData !== false;
  return (
    <div className="space-y-3">
      <div className="grid items-end gap-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_auto]">
        <Field label="Id">
          <input className={`${control.input} w-full`} value={query.id} onChange={(e) => onChange({ ...query, id: e.target.value.trim() })} spellCheck={false} />
        </Field>
        <Field label="Label">
          <input className={`${control.input} w-full`} value={query.label ?? ""} placeholder={query.id} onChange={(e) => onChange({ ...query, label: e.target.value || undefined })} />
        </Field>
        <div className="flex flex-wrap items-center gap-2">
          <label className={`${typography.micro} inline-flex cursor-pointer items-center gap-2 border border-border px-3 py-2`}>
            <input type="checkbox" checked={shown} onChange={(e) => onChange({ ...query, returnData: e.target.checked ? undefined : false })} />
            show
          </label>
          <button
            type="button"
            className={control.button}
            onClick={() =>
              isExpr
                ? onChange({ id: query.id, label: query.label, returnData: query.returnData, metricStat: defaultMetric(catalog) })
                : onChange({ id: query.id, label: query.label, returnData: query.returnData, expression: "" })
            }
          >
            {isExpr ? "Use metric" : "Use expression"}
          </button>
          {onRemove && (
            <button type="button" className={control.button} onClick={onRemove}>
              Remove
            </button>
          )}
        </div>
      </div>
      {isExpr ? (
        <Field label="Expression · other query ids with + − × ÷ and numbers">
          <input
            className={`${control.input} w-full`}
            value={query.expression ?? ""}
            placeholder="100 * e / r"
            spellCheck={false}
            onChange={(e) => onChange({ ...query, expression: e.target.value })}
          />
        </Field>
      ) : (
        <MetricPicker value={query.metricStat ?? defaultMetric(catalog)} catalog={catalog} onChange={(m) => onChange({ ...query, metricStat: m })} />
      )}
    </div>
  );
}
