/**
 * How statuses read across views: a log line's level as a tone, a log line's
 * readable text, and an alarm state's tone and label. Shared so the Logs and
 * Alarms views and the dashboard widgets always agree.
 */
import type { LogEvent } from "@/lib/api";
import type { Tone } from "@/design/tokens";
import { num } from "@/lib/format";

/** error/fatal/critical = critical, warn/warning = warning, anything else is not a status. */
export function levelTone(level?: string): Tone {
  const l = (level ?? "").toLowerCase();
  if (l === "error" || l === "fatal" || l === "critical") return "critical";
  if (l === "warn" || l === "warning") return "warning";
  return "unknown";
}

/** A log line as text plus its useful fields (JSON lines are unpacked; EMF records named). */
export function describeLog(e: LogEvent): { text: string; extra: string } {
  if (e.message.startsWith("{")) {
    try {
      const j = JSON.parse(e.message) as Record<string, unknown>;
      if (j._aws) return { text: "EMF metric record", extra: Object.keys(j).filter((k) => k !== "_aws").join(" ") };
      const text = String(j.msg ?? j.message ?? e.message);
      const extra = ["service", "route", "status", "duration_ms"]
        .filter((k) => j[k] != null)
        .map((k) => `${k}=${typeof j[k] === "number" && k === "duration_ms" ? num(j[k] as number, 1) : String(j[k])}`)
        .join(" ");
      return { text, extra };
    } catch {
      // Not JSON after all; the raw line.
    }
  }
  return { text: e.message, extra: "" };
}

/** Alarm states, written out. "" = a rule the engine has not evaluated yet. */
export const ALARM_STATE: Record<string, { tone: Tone; label: string }> = {
  OK: { tone: "positive", label: "OK" },
  ALARM: { tone: "critical", label: "In alarm" },
  INSUFFICIENT_DATA: { tone: "unknown", label: "Insufficient data" },
  "": { tone: "unknown", label: "Not evaluated yet" },
};
