/**
 * Places the services graph in columns, left to right: external peers that
 * call in come first, then every other node by its longest path from a
 * source. Cycles are broken by dropping DFS back edges for ranking only (the
 * edge is still drawn). Order within a column comes from a few barycentre
 * sweeps, which is enough to undo most crossings at this size.
 */
import type { TopoEdge, TopoNode, Topology } from "@/lib/api";

export const BOX_W = 176;
export const BOX_H = 72;
const COL_GAP = 104;
const ROW_GAP = 26;
const PAD = 10;
export const COL_PITCH = BOX_W + COL_GAP;
const ROW_PITCH = BOX_H + ROW_GAP;

export interface PlacedNode extends TopoNode {
  rank: number;
  x: number;
  y: number;
}

export interface PlacedEdge extends TopoEdge {
  key: string;
  /** Start (source's right side) and end (target's left side), spread so parallel edges do not overlap. */
  x1: number;
  y1: number;
  x2: number;
  y2: number;
  d: string;
  /** Midpoint of the curve, for the hover label. */
  mx: number;
  my: number;
}

export interface MapLayout {
  nodes: PlacedNode[];
  edges: PlacedEdge[];
  width: number;
  height: number;
  columns: number;
}

export function edgeKey(e: TopoEdge): string {
  return `${e.source}>${e.target}:${e.port}`;
}

const byId = (a: string, b: string) => {
  // "ext:other" (the overflow bucket) sinks to the bottom of its column.
  if (a === "ext:other") return 1;
  if (b === "ext:other") return -1;
  return a.localeCompare(b);
};

export function layoutTopology(topo: Topology): MapLayout {
  const edges = (topo.edges ?? []).filter((e) => e.source !== e.target);
  const nodeMap = new Map<string, TopoNode>();
  for (const n of topo.nodes ?? []) nodeMap.set(n.id, n);
  // An edge may name a node the list left out; draw it rather than drop the link.
  for (const e of edges)
    for (const id of [e.source, e.target])
      if (!nodeMap.has(id)) nodeMap.set(id, { id, label: id.replace(/^ext:/, ""), kind: id.startsWith("ext:") ? "external" : "service" });

  const ids = [...nodeMap.keys()].sort(byId);
  const out = new Map<string, string[]>(ids.map((id) => [id, []]));
  const inc = new Map<string, string[]>(ids.map((id) => [id, []]));
  for (const e of edges) {
    out.get(e.source)!.push(e.target);
    inc.get(e.target)!.push(e.source);
  }

  // External peers that open connections to us are the sources on the left.
  const callers = new Set(ids.filter((id) => nodeMap.get(id)!.kind === "external" && out.get(id)!.length > 0));

  // Drop back edges (DFS from the callers first) so longest-path ranking terminates.
  const dagPreds = new Map<string, string[]>(ids.map((id) => [id, []]));
  const state = new Map<string, 1 | 2>();
  const visit = (u: string) => {
    state.set(u, 1);
    for (const v of out.get(u)!) {
      if (callers.has(v)) continue; // an edge into a left-column caller never raises its rank
      const s = state.get(v);
      if (s === 1) continue;
      dagPreds.get(v)!.push(u);
      if (!s) visit(v);
    }
    state.set(u, 2);
  };
  for (const id of [...callers, ...ids]) if (!state.has(id)) visit(id);

  const base = callers.size > 0 ? 1 : 0;
  const rank = new Map<string, number>();
  const rankOf = (id: string): number => {
    const known = rank.get(id);
    if (known !== undefined) return known;
    let r = callers.has(id) ? 0 : base;
    if (!callers.has(id)) for (const p of dagPreds.get(id)!) r = Math.max(r, rankOf(p) + 1);
    rank.set(id, r);
    return r;
  };
  ids.forEach(rankOf);

  const columns = Math.max(0, ...rank.values()) + 1;
  const cols: string[][] = Array.from({ length: columns }, () => []);
  for (const id of ids) cols[rank.get(id)!].push(id);

  // Barycentre sweeps: centred row positions, so columns of different sizes compare.
  const pos = new Map<string, number>();
  const place = () => cols.forEach((c) => c.forEach((id, i) => pos.set(id, i - (c.length - 1) / 2)));
  place();
  const neighbours = (id: string) => [...out.get(id)!, ...inc.get(id)!];
  const sweep = (r: number, side: (other: number) => boolean) => {
    const keyed = cols[r].map((id, i) => {
      const ys = neighbours(id)
        .filter((n) => side(rank.get(n)!))
        .map((n) => pos.get(n)!);
      return { id, i, k: ys.length ? ys.reduce((a, b) => a + b, 0) / ys.length : pos.get(id)! };
    });
    keyed.sort((a, b) => a.k - b.k || a.i - b.i);
    cols[r] = keyed.map((x) => x.id);
    cols[r].forEach((id, i) => pos.set(id, i - (cols[r].length - 1) / 2));
  };
  for (let pass = 0; pass < 4; pass++) {
    for (let r = 1; r < columns; r++) sweep(r, (o) => o < r);
    for (let r = columns - 2; r >= 0; r--) sweep(r, (o) => o > r);
  }
  place();

  const tallest = Math.max(1, ...cols.map((c) => c.length));
  const height = PAD * 2 + tallest * ROW_PITCH - ROW_GAP;
  const width = PAD * 2 + columns * COL_PITCH - COL_GAP;
  const mid = PAD + ((tallest - 1) * ROW_PITCH) / 2;

  const nodes: PlacedNode[] = ids.map((id) => {
    const r = rank.get(id)!;
    return { ...nodeMap.get(id)!, rank: r, x: PAD + r * COL_PITCH, y: mid + pos.get(id)! * ROW_PITCH };
  });
  const at = new Map(nodes.map((n) => [n.id, n]));

  // Spread the ends of a node's edges down its side, ordered by the far end's height.
  const spread = (list: TopoEdge[], far: (e: TopoEdge) => string) => {
    const offs = new Map<TopoEdge, number>();
    const sorted = [...list].sort((a, b) => at.get(far(a))!.y - at.get(far(b))!.y);
    const step = Math.min(9, (BOX_H - 24) / Math.max(1, sorted.length - 1));
    sorted.forEach((e, i) => offs.set(e, (i - (sorted.length - 1) / 2) * step));
    return offs;
  };
  const outOff = new Map<TopoEdge, number>();
  const inOff = new Map<TopoEdge, number>();
  for (const id of ids) {
    spread(edges.filter((e) => e.source === id), (e) => e.target).forEach((v, k) => outOff.set(k, v));
    spread(edges.filter((e) => e.target === id), (e) => e.source).forEach((v, k) => inOff.set(k, v));
  }

  const placedEdges: PlacedEdge[] = edges.map((e) => {
    const s = at.get(e.source)!;
    const t = at.get(e.target)!;
    const x1 = s.x + BOX_W;
    const y1 = s.y + BOX_H / 2 + (outOff.get(e) ?? 0);
    const x2 = t.x;
    const y2 = t.y + BOX_H / 2 + (inOff.get(e) ?? 0);
    // Forward edges bow by half the gap; backward ones swing wider so they read as a return.
    const bend = x2 > x1 ? Math.max(COL_GAP / 2, (x2 - x1) / 2) : COL_GAP + (x1 - x2) / 4;
    const c1x = x1 + bend;
    const c2x = x2 - bend;
    const mx = 0.125 * x1 + 0.375 * c1x + 0.375 * c2x + 0.125 * x2;
    const my = 0.125 * y1 + 0.375 * y1 + 0.375 * y2 + 0.125 * y2;
    return { ...e, key: edgeKey(e), x1, y1, x2, y2, mx, my, d: `M${x1},${y1} C${c1x},${y1} ${c2x},${y2} ${x2},${y2}` };
  });

  return { nodes, edges: placedEdges, width, height, columns };
}
