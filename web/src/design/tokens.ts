/**
 * Presentation tokens, isolated so the look can change in one place.
 * Components reference these names, never raw colours.
 *
 * The look is an instrument log: hairline frames, square corners, large
 * uppercase headlines, small monospace labels, and no accent hue. Colours are
 * the semantic CSS variables of src/styles.css, so both themes need no change here.
 */

const focusRing = "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring";

/** The small monospace label used for every caption, axis and control. */
const micro = "font-mono text-[0.66rem] font-medium uppercase leading-normal tracking-[0.12em]";

export const surface = {
  muted: "text-muted-foreground",
  /** A dashed box for "nothing to show here". */
  empty: "border border-dashed border-border p-5 text-[0.95rem] leading-relaxed",
} as const;

export const typography = {
  mega: "font-display text-[clamp(2.75rem,9.5vw,9rem)] font-semibold uppercase leading-[0.84] tracking-[-0.02em] [overflow-wrap:anywhere]",
  pageTitle:
    "font-display text-[clamp(2.25rem,6vw,3.75rem)] font-semibold uppercase leading-[0.9] tracking-tight [overflow-wrap:anywhere]",
  sectionTitle: "font-display text-xl font-semibold uppercase leading-tight tracking-tight",
  rowName: "font-display text-[1.35rem] font-semibold uppercase leading-tight tracking-[-0.01em] [overflow-wrap:anywhere]",
  itemName: "font-display text-2xl font-semibold uppercase leading-[1.05] tracking-[-0.01em] [overflow-wrap:anywhere]",
  lead: "max-w-[54ch] text-[clamp(1rem,1.35vw,1.3rem)] leading-snug",
  body: "text-[0.95rem] leading-relaxed",
  micro,
  eyebrow: `${micro} text-muted-foreground`,
  number: "tabular-nums",
  stat: "font-display text-[clamp(2.4rem,5vw,4.4rem)] font-semibold leading-[0.95] tracking-[-0.02em] tabular-nums",
} as const;

const buttonBase = `inline-flex items-center justify-center gap-2 border px-3 py-2 ${micro} transition-colors ${focusRing}`;

export const control = {
  button: `${buttonBase} border-border hover:border-foreground hover:bg-foreground hover:text-background`,
  buttonActive: `${buttonBase} border-foreground bg-foreground text-background hover:bg-transparent hover:text-foreground`,
  input: `min-w-0 rounded-none border border-input bg-background px-3 py-2 font-mono text-[0.8rem] text-foreground placeholder:text-muted-foreground ${focusRing}`,
  select: `w-full min-w-0 appearance-none truncate rounded-none border border-border bg-background py-2 pl-3 pr-8 text-foreground transition-colors hover:border-foreground ${micro} ${focusRing}`,
  link: `border-b border-foreground/40 pb-0.5 transition-colors hover:border-foreground ${micro} ${focusRing}`,
} as const;

/** The page frame shared by every view. */
export const shell = {
  page: "grain min-h-screen bg-background pb-14 text-foreground",
  header: "sticky top-0 z-40 border-b border-hair bg-background",
  headerInner: "mx-auto flex w-full max-w-[115rem] flex-wrap items-center gap-x-8 gap-y-1 px-4 pt-3 sm:px-8 sm:py-3.5 lg:px-12",
  brandName: "whitespace-nowrap font-display text-[0.8rem] font-semibold uppercase tracking-[0.34em]",
  brandTagline: `hidden truncate text-muted-foreground lg:block ${micro}`,
  nav: "order-3 -mx-4 flex w-[calc(100%+2rem)] gap-5 overflow-x-auto border-t border-hair px-4 sm:order-none sm:mx-0 sm:w-auto sm:border-0 sm:px-0",
  navLink: `relative whitespace-nowrap py-2.5 text-muted-foreground transition-colors after:absolute after:inset-x-0 after:bottom-1 after:h-px after:origin-left after:scale-x-0 after:bg-foreground after:transition-transform after:duration-300 hover:text-foreground hover:after:scale-x-100 ${micro} ${focusRing}`,
  navLinkActive: "!text-foreground after:!scale-x-100",
  main: "relative mx-auto w-full max-w-[115rem] px-4 pt-8 pb-12 sm:px-8 sm:pt-12 lg:px-12",
  /** The strip fixed to the foot of the window: what is live, and how fresh. */
  status: `fixed inset-x-0 bottom-0 z-40 border-t border-hair bg-background text-muted-foreground ${micro}`,
  statusInner: "mx-auto flex h-10 w-full max-w-[115rem] items-center gap-x-6 overflow-hidden whitespace-nowrap px-4 sm:px-8 lg:px-12",
  liveDot: "mr-2 inline-block h-[0.45rem] w-[0.45rem] align-[0.02rem]",
} as const;

export const layout = {
  stack: "space-y-12 sm:space-y-16",
  sectionHeader: "mb-4 flex flex-wrap items-baseline justify-between gap-x-5 gap-y-1 border-b border-border pb-2.5",
  statGrid: "grid grid-cols-2 border-l border-t border-border lg:grid-cols-4",
  stat: "min-w-0 border-b border-r border-border p-4 transition-colors hover:bg-foreground hover:text-background sm:p-5 [&:hover_.muted]:text-background/70",
  row: "border-t border-hair py-2.5",
  frame: "relative border border-border",
} as const;

/** A status the platform computed. "unknown" (no data) is deliberately neutral: never green. */
export type Tone = "positive" | "warning" | "critical" | "unknown";

/** The square beside a status label. */
export const toneFill: Record<Tone, string> = {
  positive: "bg-tone-positive",
  warning: "bg-tone-warning",
  critical: "bg-tone-critical",
  unknown: "hatch outline outline-1 -outline-offset-1 outline-dashed outline-border",
};

/** The same tones as text, for log levels and SVG marks. */
export const toneText: Record<Tone, string> = {
  positive: "text-tone-positive",
  warning: "text-tone-warning",
  critical: "text-tone-critical",
  unknown: "text-muted-foreground",
};

/** A verdict panel or chip. */
export const tonePanel: Record<Tone, string> = {
  positive: "border border-tone-positive/50 bg-tone-positive/10",
  warning: "border border-tone-warning/55 bg-tone-warning/10",
  critical: "border border-tone-critical/50 bg-tone-critical/10",
  unknown: "hatch border border-dashed border-border",
};

/** Chart presentation: series differ by dash pattern as well as colour. */
export const chart = {
  grid: "stroke-hair",
  tick: "fill-muted-foreground font-mono text-[10px] uppercase tracking-[0.1em]",
  threshold: "stroke-foreground/75 [stroke-dasharray:6_5]",
  line: {
    primary: "fill-none stroke-foreground [stroke-linejoin:round] [stroke-linecap:round]",
    secondary: "fill-none stroke-muted-foreground [stroke-dasharray:4_3]",
  },
  swatch: {
    primary: "inline-block h-0.5 w-5 bg-foreground align-middle",
    threshold: "inline-block w-5 border-t border-dashed border-foreground align-middle",
  },
} as const;

export const skeleton = "animate-pulse bg-muted";
