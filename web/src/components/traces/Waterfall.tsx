/**
 * The waterfall: one row per span in tree order, indented by depth, with a
 * bar from its start to its end on a shared time axis that begins at the
 * trace's first span. Bars are ink in a hairline frame; failed spans are in
 * the critical tone and say "error". Subtrees collapse and expand; selecting
 * a row shows its detail. Narrow screens scroll the frame, never the page.
 */
import { useMemo } from "react";
import { plural } from "@/lib/format";
import { stagger } from "@/design/primitives";
import { chart, typography } from "@/design/tokens";
import { fmtMs, niceTicks, visibleRows, type TraceModel } from "./model";

/** Width of the name column. */
const LABEL = "w-[38%] min-w-[11rem] max-w-[26rem] shrink-0";

export function Waterfall({
  model,
  selected,
  onSelect,
  collapsed,
  onToggle,
}: {
  model: TraceModel;
  selected: string | null;
  onSelect: (spanId: string) => void;
  collapsed: ReadonlySet<string>;
  onToggle: (spanId: string) => void;
}) {
  const rows = useMemo(() => visibleRows(model.rows, collapsed), [model.rows, collapsed]);
  const ticks = useMemo(() => niceTicks(model.total), [model.total]);
  const pct = (t: number) => ((t - model.t0) / model.total) * 100;

  return (
    <div className="relative overflow-x-auto border border-border">
      <div className="min-w-[34rem]">
        {/* Time axis */}
        <div className="flex border-b border-border">
          <div className={`${LABEL} ${typography.eyebrow} border-r border-hair px-3 py-2`}>span · {plural(model.rows.length, "span")}</div>
          <div className="relative mr-3 h-8 flex-1" aria-hidden="true">
            {ticks.map((v, i) => (
              <span
                key={v}
                className={`${chart.tick} absolute top-2 whitespace-nowrap tabular-nums ${i === 0 ? "" : v / model.total > 0.92 ? "-translate-x-full" : "-translate-x-1/2"}`}
                style={{ left: `${(v / model.total) * 100}%` }}
              >
                {i === 0 ? "0" : fmtMs(v)}
              </span>
            ))}
          </div>
        </div>

        <div className="relative" role="list" aria-label="Spans in tree order">
          {/* Tick rules running down behind the bars */}
          <div className="pointer-events-none absolute inset-y-0 right-0 flex" aria-hidden="true" style={{ left: 0 }}>
            <div className={LABEL} />
            <div className="relative mr-3 flex-1">
              {ticks.map((v) => (
                <span key={v} className="absolute inset-y-0 w-px bg-hair" style={{ left: `${(v / model.total) * 100}%` }} />
              ))}
            </div>
          </div>

          {rows.map((r, i) => {
            const s = r.span;
            const id = s.spanId;
            const on = id === selected;
            const err = s.status === "error";
            const left = pct(s.start);
            const width = Math.max(0, pct(r.end) - left);
            const isCollapsed = collapsed.has(id);
            // The duration sits inside a long bar, before a bar that ends late, else after it.
            const inside = width > 28;
            const late = !inside && left + width > 72;
            const place = inside
              ? { right: `calc(${100 - left - width}% + 0.4rem)` }
              : late
                ? { right: `calc(${100 - left}% + 0.4rem)` }
                : { left: `calc(${left + width}% + 0.4rem)` };
            return (
              <div
                key={id}
                role="listitem"
                className={`relative flex cursor-pointer border-b border-hair transition-colors last:border-b-0 ${on ? "bg-muted" : "hover:bg-muted/60"}`}
                onClick={() => onSelect(id)}
              >
                {on && <span className="absolute inset-y-0 left-0 w-0.5 bg-foreground" aria-hidden="true" />}
                <div className={`${LABEL} flex min-w-0 items-center gap-1 border-r border-hair py-1.5 pr-2`} style={{ paddingLeft: `${0.5 + Math.min(r.depth, 10) * 0.85}rem` }}>
                  {r.children > 0 ? (
                    <button
                      type="button"
                      className="inline-flex h-5 w-5 shrink-0 items-center justify-center font-mono text-[0.6rem] text-muted-foreground hover:bg-foreground hover:text-background focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring"
                      aria-expanded={!isCollapsed}
                      aria-label={`${isCollapsed ? "Expand" : "Collapse"} ${plural(r.descendants, "span")} under ${s.name}`}
                      onClick={(e) => {
                        e.stopPropagation();
                        onToggle(id);
                      }}
                    >
                      {isCollapsed ? "▸" : "▾"}
                    </button>
                  ) : (
                    <span className="w-5 shrink-0" aria-hidden="true" />
                  )}
                  <button
                    type="button"
                    className="min-w-0 flex-1 text-left focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring"
                    aria-pressed={on}
                    aria-label={`${s.service} ${s.name}, ${fmtMs(r.end - s.start)}${err ? ", error" : ""}${r.orphan ? ", parent not received" : ""}`}
                  >
                    <span className={`${typography.micro} block truncate text-muted-foreground`}>
                      {s.service}
                      {isCollapsed ? ` · +${r.descendants} hidden` : ""}
                    </span>
                    <span className="block truncate text-[0.84rem] leading-snug" title={s.name}>
                      {s.name}
                    </span>
                    {r.orphan && <span className={`${typography.eyebrow} block truncate`}>parent not received</span>}
                  </button>
                </div>
                <div className="relative mr-3 flex-1" aria-hidden="true">
                  <span
                    className={`grow-x absolute top-1/2 h-3.5 -translate-y-1/2 border ${
                      err ? "border-tone-critical bg-tone-critical/35" : on ? "border-foreground bg-foreground" : "border-foreground bg-foreground/25"
                    }`}
                    style={stagger(Math.min(i, 18), { left: `${left}%`, width: `max(2px, ${width}%)` })}
                  />
                  <span
                    className={`${typography.micro} absolute top-1/2 -translate-y-1/2 whitespace-nowrap tabular-nums ${inside ? (on && !err ? "text-background" : "text-foreground") : err ? "text-tone-critical" : "text-muted-foreground"}`}
                    style={place}
                  >
                    {fmtMs(r.end - s.start)}
                    {err ? " · error" : ""}
                  </span>
                </div>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
