/**
 * The report's signature: a runway per alarm rule, from now to the end of the
 * forecast window. A solid rule runs up to where the forecast first crosses
 * the threshold and ends in a square (critical when that is in the first
 * tenth of the window, warning otherwise); a rule that is never crossed is a
 * dashed hairline marked "clear". Laid out in HTML, so labels stack above
 * their runway on phones.
 */
import type { Report } from "@/lib/api";
import { stagger } from "@/design/primitives";
import { typography } from "@/design/tokens";
import { num } from "@/lib/format";

function when(t: number, spanMs: number): string {
  const d = new Date(t);
  const time = d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
  return spanMs > 24 * 3600_000 || d.toDateString() !== new Date().toDateString()
    ? `${d.toLocaleDateString("en-GB", { day: "numeric", month: "short" })} ${time}`
    : time;
}

export function Runway({ rows, start, spanMs, label }: { rows: Report["capacity"]; start: number; spanMs: number; label: string }) {
  const ticks = [0, 0.25, 0.5, 0.75, 1];
  const tick = (f: number) => {
    if (f === 0) return "now";
    const h = (f * spanMs) / 3600_000;
    return h >= 48 ? `+${num(h / 24, 0)}d` : `+${num(h, 0)}h`;
  };
  return (
    <div className="border border-border px-4 py-3 sm:px-5" role="table" aria-label={`When each alarm threshold is forecast to be crossed, ${label}`}>
      <div className="grid gap-x-6 sm:grid-cols-[minmax(0,18rem)_minmax(0,1fr)]" role="row">
        <span className="hidden sm:block" />
        <div className={`${typography.micro} relative h-5 text-muted-foreground`} aria-hidden="true">
          {ticks.map((f) => (
            <span key={f} className="absolute top-0" style={{ left: `${f * 100}%`, transform: f === 0 ? undefined : f === 1 ? "translateX(-100%)" : "translateX(-50%)" }}>
              {tick(f)}
            </span>
          ))}
        </div>
      </div>
      {rows.map((r, i) => {
        const f = r.crossesAt != null ? Math.min(1, Math.max(0, (r.crossesAt - start) / spanMs)) : null;
        const tone = f == null ? "" : f < 0.1 ? "bg-tone-critical" : "bg-tone-warning";
        return (
          <div key={r.label + i} role="row" className="grid gap-x-6 gap-y-1 border-t border-hair py-2.5 sm:grid-cols-[minmax(0,18rem)_minmax(0,1fr)]">
            <div className="min-w-0" role="cell">
              <div className="truncate text-[0.95rem]" title={r.label}>
                {r.label}
              </div>
              <div className={`${typography.eyebrow} truncate`}>
                {r.service ? `${r.service} · ` : ""}
                {r.metric} · threshold {num(r.threshold, Math.abs(r.threshold) < 10 ? 2 : 0)}
              </div>
            </div>
            <div className="relative h-9 min-w-0" role="cell">
              {/* time grid */}
              {ticks.map((t) => (
                <span key={t} className="absolute inset-y-0 w-px bg-hair" style={{ left: `${t * 100}%` }} aria-hidden="true" />
              ))}
              {f == null ? (
                <>
                  <span className="absolute inset-x-0 top-1/2 border-t border-dashed border-border" aria-hidden="true" />
                  <span className={`${typography.micro} absolute right-0 top-0 bg-background pl-2 text-muted-foreground`}>clear for {label}</span>
                </>
              ) : (
                <>
                  <span className="absolute inset-x-0 top-1/2 h-px bg-hair" aria-hidden="true" />
                  <span className="grow-x absolute left-0 top-1/2 h-0.5 -translate-y-1/2 bg-foreground" style={stagger(i, { width: `${f * 100}%` })} aria-hidden="true" />
                  <span className={`pop absolute top-1/2 h-2.5 w-2.5 -translate-x-1/2 -translate-y-1/2 ${tone}`} style={{ left: `${f * 100}%` }} aria-hidden="true" />
                  <span
                    className={`${typography.micro} absolute top-0 whitespace-nowrap bg-background px-1`}
                    style={f > 0.6 ? { right: `${(1 - f) * 100}%`, marginRight: "0.6rem" } : { left: `${f * 100}%`, marginLeft: "0.6rem" }}
                  >
                    crosses {when(r.crossesAt!, spanMs)}
                  </span>
                </>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}
