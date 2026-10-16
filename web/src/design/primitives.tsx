/**
 * Small pieces every instrument view shares: headline lines, ruled section
 * headings, square status marks, stat cells and empty states.
 */
import type { CSSProperties, ReactNode } from "react";
import { useCountUp } from "@/lib/hooks";
import { num } from "@/lib/format";
import { layout, skeleton, surface, toneFill, tonePanel, typography, type Tone } from "./tokens";

/** A stagger position for the mount animations in src/styles.css. */
export function stagger(index: number, extra?: CSSProperties): CSSProperties {
  return { "--i": index, ...extra } as CSSProperties;
}

/** A headline set as lines that slide up one after another. Later lines are dimmer. */
export function Lines({ lines, className }: { lines: readonly string[]; className?: string }) {
  return (
    <span className={className}>
      {lines.map((line, i) => (
        <span key={`${i}${line}`} className="ln" style={stagger(i)}>
          <span className={i > 0 ? surface.muted : undefined}>{line}</span>
        </span>
      ))}
    </span>
  );
}

/** A view's opening: an eyebrow, a headline and an optional lead paragraph. */
export function PageHead({ eyebrow, lines, lead, mega }: { eyebrow: ReactNode; lines: string[]; lead?: ReactNode; mega?: boolean }) {
  return (
    <header>
      <p className={`${typography.eyebrow} rise-in mb-4`} style={stagger(0)}>
        {eyebrow}
      </p>
      <h1 className={mega ? typography.mega : typography.pageTitle}>
        <Lines lines={lines} />
      </h1>
      {lead && (
        <p className={`${typography.lead} rise-in mt-6`} style={stagger(2)}>
          {lead}
        </p>
      )}
    </header>
  );
}

/** A section: a small label, an optional note or control, a rule, then the content. */
export function Section({
  title,
  note,
  action,
  index = 0,
  className,
  children,
}: {
  title: string;
  note?: ReactNode;
  action?: ReactNode;
  index?: number;
  className?: string;
  children: ReactNode;
}) {
  return (
    <section className={`rise-in min-w-0 ${className ?? ""}`} style={stagger(index)} aria-label={title}>
      <div className={layout.sectionHeader}>
        <h2 className={typography.micro}>{title}</h2>
        {note && <span className={typography.eyebrow}>{note}</span>}
        {action}
      </div>
      {children}
    </section>
  );
}

/** A status as a square and its label. The label is written out; colour is never the only signal. */
export function StatusMark({ tone, label, className }: { tone: Tone; label: string; className?: string }) {
  return (
    <span className={`inline-flex items-center gap-2 whitespace-nowrap ${typography.micro} ${tone === "unknown" ? surface.muted : ""} ${className ?? ""}`}>
      <span className={`inline-block h-2.5 w-2.5 shrink-0 ${toneFill[tone]}`} aria-hidden="true" />
      {label}
    </span>
  );
}

/** A verdict panel: the status mark inside a tinted frame (hatched when unknown). */
export function Verdict({ tone, label }: { tone: Tone; label: string }) {
  return (
    <span className={`inline-flex px-2.5 py-1.5 ${tonePanel[tone]}`}>
      <StatusMark tone={tone} label={label} />
    </span>
  );
}

/** One cell of a stat grid: a counted number, a label above and a unit below. */
export function Stat({ label, value, decimals = 0, unit, title }: { label: string; value: number | null; decimals?: number; unit: string; title?: string }) {
  const shown = useCountUp(value ?? 0);
  return (
    <div className={layout.stat} title={title}>
      <div className={`${typography.micro} muted ${surface.muted}`}>{label}</div>
      <div className={`${typography.stat} my-2`}>{value == null ? "—" : num(shown, decimals)}</div>
      <div className={`${typography.micro} muted ${surface.muted}`}>{value == null ? "no data in window" : unit}</div>
    </div>
  );
}

export function StatGrid({ children }: { children: ReactNode }) {
  return <div className={layout.statGrid}>{children}</div>;
}

/** A plain sentence in a dashed box, hatched when it means "nothing measured". */
export function Empty({ children, hatch }: { children: ReactNode; hatch?: boolean }) {
  return <div className={`${surface.empty} ${hatch ? "hatch" : ""}`}>{children}</div>;
}

export function Skeleton({ className }: { className: string }) {
  return <div className={`${skeleton} ${className}`} aria-hidden="true" />;
}

/** A failed request, said plainly with what to try. */
export function ErrorNote({ error, what }: { error: Error; what: string }) {
  return (
    <Empty>
      Could not load {what}: <span className="font-mono text-[0.8rem]">{error.message}</span>. It will retry on its own.
    </Empty>
  );
}
