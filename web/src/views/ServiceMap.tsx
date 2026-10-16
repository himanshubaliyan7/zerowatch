/**
 * Who talks to whom: the supervisor's topology (from the host TCP table)
 * drawn left to right, with each service's RED numbers joined on by name.
 * Selecting a node lists its callers and callees; the Links table below says
 * the same as the drawing in plain rows.
 */
import { useMemo, useState } from "react";
import { endpoints, ApiError, RED_NAMESPACE, type MetricStat, type Topology } from "@/lib/api";
import { usePoll } from "@/lib/hooks";
import { fetchRed, listServices, WINDOW_MIN, type ServiceRed } from "@/lib/red";
import { ms, num, plural } from "@/lib/format";
import { Empty, ErrorNote, PageHead, Section, Skeleton, Stat, StatGrid, StatusMark, stagger } from "@/design/primitives";
import { control, typography, type Tone } from "@/design/tokens";
import { layoutTopology, type MapLayout, type PlacedEdge, type PlacedNode } from "@/components/map/layout";
import { MapDrawing, type NodeInfo } from "@/components/map/MapDrawing";

export function ServiceMap() {
  const sup = usePoll(() => endpoints.supervisor(), 10_000);
  const services = usePoll(() => listServices(), 30_000);
  const rules = usePoll(() => endpoints.alarms(), 10_000);
  const names = services.data ?? [];
  const red = usePoll((signal) => fetchRed(names, rules.data ?? [], signal), 10_000, [names.join(","), rules.data]);

  if (sup.error instanceof ApiError && sup.error.status === 404) {
    return (
      <div className="space-y-12">
        <PageHead eyebrow="Service map" lines={["No map.", "The supervisor is off."]} />
        <Empty hatch>
          The map is read from the host's TCP table by the autonomous supervisor, and this node runs without it. Start ZeroWatch with
          <span className="font-mono text-[0.8rem]"> --supervisor=true</span> (on by default for <span className="font-mono text-[0.8rem]">--role=all</span>).
        </Empty>
      </div>
    );
  }

  const s = sup.data;
  const topo = s?.topology;
  const edgeCount = topo?.edges?.length ?? 0;
  const serviceCount = (topo?.nodes ?? []).filter((n) => n.kind === "service").length;
  const eyebrow = s ? `Service map · from the host TCP table · cycle ${num(s.cycle)}` : "Service map";
  const lines = s && edgeCount > 0 ? [`${plural(serviceCount, "service")}.`, `${plural(edgeCount, "link")}.`] : ["Who talks to whom."];

  return (
    <div className="space-y-12 sm:space-y-16">
      <PageHead eyebrow={eyebrow} lines={lines} lead={topo && edgeCount > 0 ? leadFor(topo) : undefined} />
      {sup.error && !s ? (
        <ErrorNote error={sup.error} what="the service map" />
      ) : !s ? (
        <div className="space-y-4" aria-busy="true">
          <Skeleton className="h-4 w-48" />
          <Skeleton className="h-80 w-full" />
        </div>
      ) : !topo || edgeCount === 0 ? (
        <Empty hatch>
          No links seen yet. The map fills in once services talk to each other on a Linux host: the supervisor reads who is connected to whom
          from the TCP table every cycle. The synthetic generator alone writes metrics but opens no connections, so it draws no links.
        </Empty>
      ) : (
        <MapBody topo={topo} red={red.data?.services ?? []} redError={red.error} />
      )}
    </div>
  );
}

/** One sentence from the data: who calls in, and who fans out the most. */
function leadFor(topo: Topology): string {
  const edges = topo.edges ?? [];
  const nodes = topo.nodes ?? [];
  const ext = nodes.filter((n) => n.kind === "external" && edges.some((e) => e.source === n.id)).length;
  const fan = new Map<string, number>();
  for (const e of edges) if (!e.source.startsWith("ext:")) fan.set(e.source, (fan.get(e.source) ?? 0) + 1);
  const top = [...fan.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))[0];
  const conns = edges.reduce((a, e) => a + e.connections, 0);
  const parts = [
    ext ? `${plural(ext, "external peer")} call${ext === 1 ? "s" : ""} in` : "Nothing calls in from outside",
    top ? `${top[0]} calls the most services (${top[1]})` : "no service calls another",
  ];
  return `${parts.join("; ")}. ${plural(conns, "open connection")} in all.`;
}

function infoFor(n: PlacedNode, r: ServiceRed | undefined, layout: MapLayout): NodeInfo {
  if (n.kind === "external") {
    const outConn = layout.edges.filter((e) => e.source === n.id).reduce((a, e) => a + e.connections, 0);
    const inConn = layout.edges.filter((e) => e.target === n.id).reduce((a, e) => a + e.connections, 0);
    const detail = outConn ? `${num(outConn)} conn in` : `${num(inConn)} conn out`;
    return { tone: "unknown", status: "External peer", detail, spoken: `${n.label}, external peer, ${detail.replace("conn", "connections")}` };
  }
  if (!r || r.rps == null) {
    const tone: Tone = r && r.alarms.length ? "critical" : "unknown";
    const status = r ? r.status : "No RED data";
    const detail = n.ports?.length ? `:${n.ports.join(" :")}${n.pid ? ` · pid ${n.pid}` : ""}` : n.pid ? `pid ${n.pid}` : "not probed";
    return { tone, status, detail, spoken: `${n.label}, ${status}` };
  }
  const detail = `${num(r.rps, r.rps < 10 ? 1 : 0)} req/s · p99 ${ms(r.p99)}`;
  return {
    tone: r.tone,
    status: r.status,
    detail,
    spoken: `${n.label}, ${r.status}, ${num(r.rps, 1)} requests per second, p99 ${ms(r.p99)}`,
  };
}

function MapBody({ topo, red, redError }: { topo: Topology; red: ServiceRed[]; redError: Error | undefined }) {
  const layout = useMemo(() => layoutTopology(topo), [topo]);
  const redBy = useMemo(() => new Map(red.map((r) => [r.name, r])), [red]);
  const info = useMemo(() => new Map(layout.nodes.map((n) => [n.id, infoFor(n, redBy.get(n.id), layout)])), [layout, redBy]);
  const [picked, setPicked] = useState<string | null>(null);
  // A node that left the topology since it was picked is simply no longer selected.
  const selected = picked && layout.nodes.some((n) => n.id === picked) ? picked : null;

  const tones = new Map<Tone, number>();
  for (const n of layout.nodes) if (n.kind === "service") tones.set(info.get(n.id)!.tone, (tones.get(info.get(n.id)!.tone) ?? 0) + 1);
  const note = [
    tones.get("critical") ? `${num(tones.get("critical"))} in alarm` : null,
    tones.get("warning") ? `${num(tones.get("warning"))} degraded` : null,
    `RED over the last ${WINDOW_MIN} min`,
  ]
    .filter(Boolean)
    .join(" · ");

  const node = selected ? layout.nodes.find((n) => n.id === selected) : undefined;
  const links = [...layout.edges].sort((a, b) => b.connections - a.connections || a.key.localeCompare(b.key));

  return (
    <>
      <Section index={1} title="Map" note={note}>
        {redError && <p className={`${typography.eyebrow} mb-3`}>RED numbers unavailable: {redError.message}. The links still show.</p>}
        <div className="relative overflow-x-auto border border-border">
          <div className="wipe w-max p-3 sm:p-5">
            <MapDrawing layout={layout} info={info} selected={selected} onSelect={setPicked} />
          </div>
        </div>
        <Legend />
      </Section>

      {node ? (
        <Detail key={node.id} node={node} info={info.get(node.id)!} r={redBy.get(node.id)} layout={layout} onSelect={setPicked} />
      ) : (
        <p className={`${typography.eyebrow} rise-in`} style={stagger(2)}>
          Select a service (click, or Tab and Enter) to see its callers, callees and RED numbers.
        </p>
      )}

      <Section index={3} title="Links" note={`${plural(links.length, "link")} · busiest first`}>
        <div className="relative overflow-x-auto">
          <table className="w-full min-w-[30rem] text-left text-[0.95rem]">
            <thead>
              <tr className={typography.eyebrow}>
                <th className="pb-2.5 pr-3 font-medium">Source</th>
                <th className="w-10 pb-2.5 pr-3 font-medium" aria-hidden="true" />
                <th className="pb-2.5 pr-3 font-medium">Target</th>
                <th className="pb-2.5 pr-3 text-right font-medium">Port</th>
                <th className="pb-2.5 text-right font-medium">Connections</th>
              </tr>
            </thead>
            <tbody>
              {links.map((e) => {
                const on = selected != null && (e.source === selected || e.target === selected);
                return (
                  <tr key={e.key} className={`border-t border-hair transition-colors ${on ? "bg-muted" : ""}`}>
                    <td className="py-2.5 pr-3">
                      <NodeName id={e.source} layout={layout} onSelect={setPicked} />
                    </td>
                    <td className="py-2.5 pr-3 text-muted-foreground" aria-label="calls">
                      →
                    </td>
                    <td className="py-2.5 pr-3">
                      <NodeName id={e.target} layout={layout} onSelect={setPicked} />
                    </td>
                    <td className="py-2.5 pr-3 text-right font-mono text-[0.8rem] tabular-nums">{e.port}</td>
                    <td className="py-2.5 text-right tabular-nums">{num(e.connections)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Section>
    </>
  );
}

function NodeName({ id, layout, onSelect }: { id: string; layout: MapLayout; onSelect: (id: string) => void }) {
  const n = layout.nodes.find((x) => x.id === id);
  const ext = n?.kind === "external";
  return (
    <button
      type="button"
      onClick={() => onSelect(id)}
      className={`text-left transition-colors hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring ${
        ext ? "font-mono text-[0.8rem] text-muted-foreground" : "font-display font-semibold uppercase"
      } [overflow-wrap:anywhere]`}
    >
      {n?.label || id}
    </button>
  );
}

function Legend() {
  return (
    <div className={`${typography.eyebrow} mt-3 flex flex-wrap items-center gap-x-6 gap-y-2`}>
      <span className="inline-flex items-center gap-2">
        <svg width="28" height="8" aria-hidden="true">
          <line x1="0" y1="2" x2="28" y2="2" className="stroke-foreground/50" strokeWidth="1" strokeDasharray="5 7" />
          <line x1="0" y1="6" x2="28" y2="6" className="stroke-foreground/50" strokeWidth="3" strokeDasharray="5 7" />
        </svg>
        line width = open connections
      </span>
      <span className="inline-flex items-center gap-2">
        <span className="hatch inline-block h-2.5 w-4 border border-dashed border-border" aria-hidden="true" />
        external peer
      </span>
      <StatusMark tone="positive" label="OK" />
      <StatusMark tone="warning" label="Degraded" />
      <StatusMark tone="critical" label="In alarm" />
      <StatusMark tone="unknown" label="No data" />
      <span className="sm:hidden">scroll the map sideways</span>
    </div>
  );
}

function Detail({
  node,
  info,
  r,
  layout,
  onSelect,
}: {
  node: PlacedNode;
  info: NodeInfo;
  r: ServiceRed | undefined;
  layout: MapLayout;
  onSelect: (id: string | null) => void;
}) {
  const callers = layout.edges.filter((e) => e.target === node.id);
  const callees = layout.edges.filter((e) => e.source === node.id);
  const conns = [...callers, ...callees].reduce((a, e) => a + e.connections, 0);
  const ext = node.kind === "external";
  const p99: MetricStat = { namespace: RED_NAMESPACE, metricName: "Latency", stat: "p99", dimensions: { Route: "*", Service: node.id } };

  return (
    <Section
      index={2}
      title={`Selected · ${node.label || node.id}`}
      action={
        <button type="button" className={control.button} onClick={() => onSelect(null)}>
          Clear
        </button>
      }
    >
      <div className="space-y-8">
        <div className="min-w-0 space-y-4">
          <div className="flex flex-wrap items-baseline gap-x-5 gap-y-2">
            <h3 className={typography.itemName}>{node.label || node.id}</h3>
            <StatusMark tone={info.tone} label={info.status} />
            {node.ports?.length ? <span className={typography.eyebrow}>port {node.ports.join(", ")}</span> : null}
            {node.pid ? <span className={typography.eyebrow}>pid {node.pid}</span> : null}
          </div>
          {ext ? (
            <Empty hatch>An address outside this host. ZeroWatch sees its connections but has no RED numbers for it.</Empty>
          ) : (
            <StatGrid>
              <Stat label="Requests" value={r?.rps ?? null} decimals={1} unit="per second" />
              <Stat label="Errors" value={r?.errorPct ?? null} decimals={2} unit="% 5xx" />
              <Stat label="Latency p99" value={r?.p99 ?? null} unit="ms" />
              <Stat label="Connections" value={conns} unit="open, in and out" />
            </StatGrid>
          )}
          {r && r.alarms.length > 0 && <p className={typography.body}>In alarm: {r.alarms.join(", ")}.</p>}
          {!ext && (
            <div className="flex flex-wrap gap-x-6 gap-y-3 pt-1">
              <a className={control.link} href={`#/logs?q=service%3D${encodeURIComponent(node.id)}`}>
                Logs for {node.label || node.id} →
              </a>
              <a className={control.link} href={`#/traces?service=${encodeURIComponent(node.id)}`}>
                Traces for {node.label || node.id} →
              </a>
              <a className={control.link} href={`#/explore?m=${encodeURIComponent(JSON.stringify(p99))}`}>
                Explore p99 →
              </a>
            </div>
          )}
        </div>
        <div className="grid min-w-0 gap-8 md:grid-cols-2">
          <PeerList title="Callers" empty="Nothing calls it." edges={callers} far={(e) => e.source} layout={layout} onSelect={onSelect} />
          <PeerList title="Callees" empty="It calls nothing we can see." edges={callees} far={(e) => e.target} layout={layout} onSelect={onSelect} />
        </div>
      </div>
    </Section>
  );
}

function PeerList({
  title,
  empty,
  edges,
  far,
  layout,
  onSelect,
}: {
  title: string;
  empty: string;
  edges: PlacedEdge[];
  far: (e: PlacedEdge) => string;
  layout: MapLayout;
  onSelect: (id: string) => void;
}) {
  const sorted = [...edges].sort((a, b) => b.connections - a.connections);
  return (
    <div className="min-w-0">
      <h4 className={`${typography.eyebrow} pb-2.5`}>
        {title} · {num(edges.length)}
      </h4>
      {sorted.length === 0 ? (
        <p className={`border-t border-hair py-2.5 text-[0.95rem] text-muted-foreground`}>{empty}</p>
      ) : (
        <ul className="border-b border-hair">
          {sorted.map((e, i) => (
            <li key={e.key} className="row-in grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-4 border-t border-hair py-2.5" style={stagger(i)}>
              <NodeName id={far(e)} layout={layout} onSelect={onSelect} />
              <span className={`${typography.eyebrow} tabular-nums`}>
                :{e.port} · {num(e.connections)} conn
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
