const nf = (d: number) => new Intl.NumberFormat("en-GB", { maximumFractionDigits: d, minimumFractionDigits: d });
const nf0 = nf(0);
const nf1 = nf(1);

/** Thin-space thousands, fixed decimals: 1 284, 6.4 */
export function num(v: number | null | undefined, decimals = 0): string {
  if (v == null || !Number.isFinite(v)) return "—";
  const s = (decimals === 0 ? nf0 : decimals === 1 ? nf1 : nf(decimals)).format(v);
  return s.replace(/,/g, " ");
}

export function ms(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return "—";
  return v >= 10_000 ? `${num(v / 1000, 1)} s` : `${num(v)} ms`;
}

export function clock(t: number | string, withMs = false): string {
  const d = new Date(t);
  if (Number.isNaN(d.getTime())) return "—";
  const s = d.toLocaleTimeString("en-GB", { hour12: false });
  return withMs ? `${s}.${String(d.getMilliseconds()).padStart(3, "0")}` : s;
}

export function ago(t: number | string, now = Date.now()): string {
  const d = typeof t === "number" ? t : new Date(t).getTime();
  if (!Number.isFinite(d) || d <= 0) return "never";
  const s = Math.max(0, Math.round((now - d) / 1000));
  if (s < 2) return "just now";
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  return `${Math.floor(s / 3600)} h ago`;
}

export function bytes(n: number): string {
  if (!Number.isFinite(n)) return "—";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024;
    i++;
  }
  return `${num(n, i === 0 ? 0 : 1)} ${u[i]}`;
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${num(n)} ${n === 1 ? one : many}`;
}

const COMPARE: Record<string, string> = {
  GreaterThanThreshold: ">",
  GreaterThanOrEqualToThreshold: "≥",
  LessThanThreshold: "<",
  LessThanOrEqualToThreshold: "≤",
};

export function comparison(c: string): string {
  return COMPARE[c] ?? c;
}

export function breaches(value: number, comparisonName: string, threshold: number): boolean {
  switch (comparisonName) {
    case "GreaterThanThreshold":
      return value > threshold;
    case "GreaterThanOrEqualToThreshold":
      return value >= threshold;
    case "LessThanThreshold":
      return value < threshold;
    case "LessThanOrEqualToThreshold":
      return value <= threshold;
    default:
      return false;
  }
}
