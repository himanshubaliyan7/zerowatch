/**
 * Traces: search (#/traces, #/traces?service=X) with a duration scatter and a
 * ruled list, or one trace (#/traces?id=<traceId>) as a waterfall of its spans.
 */
import { TraceSearch } from "@/components/traces/TraceSearch";
import { TraceView } from "@/components/traces/TraceView";

export function Traces({ id, service }: { id?: string; service?: string }) {
  return id ? <TraceView id={id} /> : <TraceSearch service={service} />;
}
