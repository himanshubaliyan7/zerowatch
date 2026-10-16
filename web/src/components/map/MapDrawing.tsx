/**
 * The services graph as hand-drawn SVG: square hairline frames for nodes,
 * marching cubic paths for links (width by connection count). Colour appears
 * only as a node's status, always beside its written label.
 */
import { useId, useState, type CSSProperties, type KeyboardEvent } from "react";
import type { Tone } from "@/design/tokens";
import { BOX_H, BOX_W, type MapLayout, type PlacedEdge, type PlacedNode } from "./layout";

export interface NodeInfo {
  tone: Tone;
  status: string;
  /** The mono line under the status: "12.4 req/s · p99 85 ms". */
  detail: string;
  /** Spoken summary for the node's button. */
  spoken: string;
}

const toneStroke: Record<Tone, string> = {
  positive: "stroke-border",
  warning: "stroke-tone-warning",
  critical: "stroke-tone-critical",
  unknown: "stroke-border",
};
const toneSquare: Record<Exclude<Tone, "unknown">, string> = {
  positive: "fill-tone-positive",
  warning: "fill-tone-warning",
  critical: "fill-tone-critical",
};

const micro = "font-mono text-[9.5px] font-medium uppercase tracking-[0.12em]";

function clip(s: string, max: number): string {
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

export function MapDrawing({
  layout,
  info,
  selected,
  onSelect,
}: {
  layout: MapLayout;
  info: Map<string, NodeInfo>;
  selected: string | null;
  onSelect: (id: string | null) => void;
}) {
  const hatchId = useId().replace(/:/g, "");
  const [hover, setHover] = useState<string | null>(null);
  const maxConn = Math.max(1, ...layout.edges.map((e) => e.connections));
  const minConn = Math.min(maxConn, ...layout.edges.map((e) => e.connections));
  const strokeW = (c: number) => (maxConn === minConn ? 1.5 : 1 + (2 * (c - minConn)) / (maxConn - minConn));

  const touches = (e: PlacedEdge) => selected != null && (e.source === selected || e.target === selected);
  const near = new Set<string>();
  if (selected) {
    near.add(selected);
    for (const e of layout.edges) if (touches(e)) near.add(e.source === selected ? e.target : e.source);
  }
  const labelled = layout.edges.filter((e) => e.key === hover);

  const activate = (id: string) => onSelect(selected === id ? null : id);
  const onKey = (ev: KeyboardEvent, id: string) => {
    if (ev.key === "Enter" || ev.key === " ") {
      ev.preventDefault();
      activate(id);
    }
  };

  return (
    <svg
      width={layout.width}
      height={layout.height}
      viewBox={`0 0 ${layout.width} ${layout.height}`}
      className="block max-w-none"
      role="group"
      aria-label="Services drawing. The Links table below lists the same links."
      onKeyDown={(ev) => ev.key === "Escape" && onSelect(null)}
    >
      <defs>
        <pattern id={hatchId} width="7" height="7" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
          <line x1="0" y1="0" x2="0" y2="7" className="stroke-border" strokeWidth="1" />
        </pattern>
      </defs>

      <g>
        {layout.edges.map((e) => {
          const lit = touches(e) || e.key === hover;
          const dim = selected != null && !touches(e);
          return (
            <g
              key={e.key}
              tabIndex={0}
              className="group cursor-default outline-none"
              aria-label={`${e.source.replace(/^ext:/, "")} to ${e.target}, port ${e.port}, ${e.connections} connections`}
              onMouseEnter={() => setHover(e.key)}
              onMouseLeave={() => setHover((h) => (h === e.key ? null : h))}
              onFocus={() => setHover(e.key)}
              onBlur={() => setHover((h) => (h === e.key ? null : h))}
            >
              <path d={e.d} className="fill-none stroke-transparent" strokeWidth={14} />
              <path
                d={e.d}
                className={`flow-path fill-none transition-[stroke,opacity] duration-300 ${lit ? "stroke-foreground" : "stroke-foreground/60"} ${dim ? "opacity-15" : ""}`}
                strokeWidth={strokeW(e.connections)}
                strokeLinecap="butt"
              />
              <circle cx={e.x2} cy={e.y2} r={2} className={`fill-foreground ${dim ? "opacity-15" : ""}`} aria-hidden="true" />
            </g>
          );
        })}
      </g>

      <g>
        {layout.nodes.map((n, i) => (
          <Node
            key={n.id}
            n={n}
            index={i}
            info={info.get(n.id)}
            hatchId={hatchId}
            selected={selected === n.id}
            dim={selected != null && !near.has(n.id)}
            onActivate={() => activate(n.id)}
            onKey={(ev) => onKey(ev, n.id)}
          />
        ))}
      </g>

      <g aria-hidden="true" className="pointer-events-none">
        {labelled.map((e) => {
          const text = `${e.port} · ${e.connections} conn`;
          const w = text.length * 6.9 + 12;
          return (
            <g key={e.key} transform={`translate(${e.mx - w / 2},${e.my - 9})`}>
              <rect width={w} height={18} className="fill-background stroke-foreground" strokeWidth={1} />
              <text x={w / 2} y={12.5} textAnchor="middle" className={`${micro} fill-foreground`}>
                {text}
              </text>
            </g>
          );
        })}
      </g>
    </svg>
  );
}

function Node({
  n,
  index,
  info,
  hatchId,
  selected,
  dim,
  onActivate,
  onKey,
}: {
  n: PlacedNode;
  index: number;
  info: NodeInfo | undefined;
  hatchId: string;
  selected: boolean;
  dim: boolean;
  onActivate: () => void;
  onKey: (ev: KeyboardEvent) => void;
}) {
  const ext = n.kind === "external";
  const tone: Tone = info?.tone ?? "unknown";
  const name = ext ? n.label : n.label || n.id;
  return (
    <g transform={`translate(${n.x},${n.y})`}>
      <g
        role="button"
        tabIndex={0}
        aria-pressed={selected}
        aria-label={info?.spoken ?? name}
        className={`pop group cursor-pointer outline-none transition-opacity duration-300 ${dim ? "opacity-35" : ""}`}
        style={{ "--i": index, transformBox: "fill-box", transformOrigin: "center" } as CSSProperties}
        onClick={onActivate}
        onKeyDown={onKey}
      >
        <title>{name}</title>
        <rect width={BOX_W} height={BOX_H} className="fill-background" />
        {ext && <rect width={BOX_W} height={BOX_H} fill={`url(#${hatchId})`} />}
        <rect
          x={0.5}
          y={0.5}
          width={BOX_W - 1}
          height={BOX_H - 1}
          className={`fill-none transition-[stroke] duration-200 ${selected ? "stroke-foreground" : `${toneStroke[tone]} group-hover:stroke-foreground`}`}
          strokeWidth={selected || tone === "critical" || tone === "warning" ? 1.5 : 1}
          strokeDasharray={ext ? "4 3" : undefined}
        />
        {/* Focus ring, drawn outside the frame like the page's :focus-visible outline. */}
        <rect x={-4} y={-4} width={BOX_W + 8} height={BOX_H + 8} className="fill-none stroke-foreground opacity-0 group-focus-visible:opacity-100" strokeWidth={2} />

        {ext ? (
          <text x={12} y={22} className="fill-foreground font-mono text-[11px] font-medium tracking-[0.04em]">
            {clip(name, 22)}
          </text>
        ) : (
          <text x={12} y={23} className="fill-foreground font-display text-[14px] font-semibold uppercase tracking-[-0.01em]">
            {clip(name, 16)}
          </text>
        )}

        {ext || tone === "unknown" ? (
          <rect x={12.5} y={34.5} width={8} height={8} fill={`url(#${hatchId})`} className="stroke-border" strokeDasharray="2 1.5" strokeWidth={1} />
        ) : (
          <rect x={12} y={34} width={9} height={9} className={toneSquare[tone]} />
        )}
        <text x={28} y={42} className={`${micro} ${tone === "unknown" || ext ? "fill-muted-foreground" : "fill-foreground"}`}>
          {clip(info?.status ?? (ext ? "External peer" : "No data"), 20)}
        </text>
        <text x={12} y={60} className={`${micro} fill-muted-foreground tabular-nums`}>
          {clip(info?.detail ?? "", 23)}
        </text>
      </g>
    </g>
  );
}
