/**
 * Dashboards: the list of saved boards, and one board drawn on a 12-column
 * grid (stacked full width on phones). Edit mode is a plain builder; saving is
 * compare-and-swap on the board's version, so two editors never overwrite
 * each other silently.
 */
import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties } from "react";
import { ApiError, endpoints, type Dashboard } from "@/lib/api";
import { useDebounced, useNow, usePoll, useRoute } from "@/lib/hooks";
import { ago, plural } from "@/lib/format";
import { RANGES } from "@/lib/range";
import { Empty, ErrorNote, PageHead, Section, Skeleton, stagger } from "@/design/primitives";
import { control, typography } from "@/design/tokens";
import { useSeriesCatalog } from "@/components/MetricPicker";
import { WidgetBody, WidgetSection } from "@/components/dashboard/Widgets";
import { BoardSettings, LayoutMap, WidgetEditor, newWidget } from "@/components/dashboard/Editor";
import { DEFAULT_REFRESH, MAX_WIDGETS, boardRange, boardRefresh, readingOrder, validateBoard } from "@/components/dashboard/model";

export function Dashboards({ id }: { id?: string }) {
  return id ? <Board key={id} id={id} /> : <BoardList />;
}

// ---- list ----

function BoardList() {
  const boards = usePoll(() => endpoints.dashboards(), 30_000);
  const now = useNow(10_000);
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<Error>();

  const list = [...(boards.data ?? [])].sort((a, b) => a.title.localeCompare(b.title));
  const headline = !boards.data ? "Looking…" : list.length === 0 ? "No dashboards yet." : list.length === 1 ? "One dashboard." : `${plural(list.length, "dashboard")}.`;

  const create = async () => {
    setCreating(true);
    setCreateError(undefined);
    try {
      const d = await endpoints.saveDashboard({ title: "Untitled board", widgets: [], layout: { range: "1h", refresh: DEFAULT_REFRESH } });
      window.location.hash = `#/dashboards?id=${encodeURIComponent(d.id ?? "")}&edit=1`;
    } catch (e) {
      setCreateError(e instanceof Error ? e : new Error(String(e)));
      setCreating(false);
    }
  };

  return (
    <div className="space-y-12 sm:space-y-16">
      <PageHead
        eyebrow="Saved boards · the supervisor writes one for the services it found"
        lines={[headline]}
        lead="Boards are grids of charts, stats, anomaly bands, logs and alarms over one time range. Open one to read it, or start a new one and build it here."
      />
      <Section
        index={3}
        title="Boards"
        note="title · widgets · last saved"
        action={
          <button type="button" className={control.buttonActive} onClick={create} disabled={creating}>
            {creating ? "Creating…" : "New dashboard"}
          </button>
        }
      >
        {createError && (
          <div className="mb-4">
            <Empty>
              Could not create the board: <span className="font-mono text-[0.8rem]">{createError.message}</span>.
            </Empty>
          </div>
        )}
        {boards.error && !boards.data ? (
          <ErrorNote error={boards.error} what="dashboards" />
        ) : !boards.data ? (
          <div className="space-y-2">
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} className="h-16 w-full" />
            ))}
          </div>
        ) : list.length === 0 ? (
          <Empty>No boards are saved. Create one with "New dashboard"; the supervisor also writes an Auto-RED board once it has found services.</Empty>
        ) : (
          <ul className="border-b border-hair">
            {list.map((b, i) => (
              <li key={b.id} className="rise-in" style={stagger(i + 4)}>
                <a
                  href={`#/dashboards?id=${encodeURIComponent(b.id)}`}
                  className="grid gap-x-6 gap-y-1 border-t border-hair py-5 transition-[background-color,padding] duration-300 hover:bg-muted hover:pl-4 sm:grid-cols-[minmax(0,1fr)_auto_auto] sm:items-baseline sm:py-7"
                >
                  <span className={typography.itemName}>{b.title}</span>
                  <span className={`${typography.eyebrow} tabular-nums`}>{plural(b.widgets, "widget")}</span>
                  <span className={`${typography.eyebrow} tabular-nums`}>saved {ago(b.updatedAt, now)}</span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </Section>
    </div>
  );
}

// ---- one board ----

type Load = { state: "loading" } | { state: "error"; error: Error } | { state: "missing" } | { state: "ok"; board: Dashboard };

function useBoard(id: string) {
  const [load, setLoad] = useState<Load>({ state: "loading" });
  const [nonce, setNonce] = useState(0);
  useEffect(() => {
    let alive = true;
    endpoints
      .dashboard(id)
      .then((board) => alive && setLoad({ state: "ok", board: { ...board, widgets: board.widgets ?? [] } }))
      .catch((e: unknown) => {
        if (!alive) return;
        if (e instanceof ApiError && e.status === 404) setLoad({ state: "missing" });
        else setLoad({ state: "error", error: e instanceof Error ? e : new Error(String(e)) });
      });
    return () => {
      alive = false;
    };
  }, [id, nonce]);
  const reload = useCallback(() => setNonce((n) => n + 1), []);
  return { load, setLoad, reload };
}

/** Drop "&edit=1" from the address without a hashchange (which would remount the view). */
function clearEditParam(id: string) {
  try {
    window.history.replaceState(null, "", `#/dashboards?id=${encodeURIComponent(id)}`);
  } catch {
    // Without history access the parameter just stays; harmless.
  }
}

function Board({ id }: { id: string }) {
  const { params } = useRoute();
  const { load, setLoad, reload } = useBoard(id);
  const [draft, setDraft] = useState<Dashboard | null>(null);
  const [rangeOverride, setRangeOverride] = useState<string>();
  const [saving, setSaving] = useState(false);
  const [problems, setProblems] = useState<string[]>([]);
  const [saveError, setSaveError] = useState<string>();
  const [conflict, setConflict] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [deleteError, setDeleteError] = useState<string>();
  const openedEdit = useRef(false);

  const board = load.state === "ok" ? load.board : undefined;

  useEffect(() => {
    if (board && !openedEdit.current && params.get("edit") === "1") {
      openedEdit.current = true;
      setDraft(structuredClone(board));
    }
  }, [board, params]);

  if (load.state === "loading") {
    return (
      <div className="space-y-8">
        <Skeleton className="h-4 w-48" />
        <Skeleton className="h-16 w-3/4" />
        <div className="grid gap-6 md:grid-cols-12">
          <Skeleton className="h-48 md:col-span-4" />
          <Skeleton className="h-48 md:col-span-8" />
        </div>
      </div>
    );
  }
  if (load.state === "missing") {
    return (
      <div className="space-y-8">
        <BackLink />
        <Empty>This dashboard no longer exists.</Empty>
      </div>
    );
  }
  if (load.state === "error") {
    return (
      <div className="space-y-8">
        <BackLink />
        <Empty>
          Could not load this dashboard: <span className="font-mono text-[0.8rem]">{load.error.message}</span>.{" "}
          <button type="button" className={control.link} onClick={reload}>
            Try again
          </button>
        </Empty>
      </div>
    );
  }

  const b = load.board;
  const shown = draft ?? b;
  const rangeId = rangeOverride ?? boardRange(shown);
  const refresh = boardRefresh(shown);

  const startEdit = () => {
    setDraft(structuredClone(b));
    setProblems([]);
    setSaveError(undefined);
    setConflict(false);
  };
  const cancelEdit = () => {
    setDraft(null);
    setProblems([]);
    setSaveError(undefined);
    setConflict(false);
    setConfirmDelete(false);
    clearEditParam(id);
  };
  const save = async () => {
    if (!draft) return;
    const errs = validateBoard(draft);
    setProblems(errs);
    setSaveError(undefined);
    if (errs.length) return;
    setSaving(true);
    try {
      const saved = await endpoints.saveDashboard({ ...draft, title: draft.title.trim(), id: b.id, version: b.version });
      setLoad({ state: "ok", board: { ...saved, widgets: saved.widgets ?? [] } });
      setDraft(null);
      setRangeOverride(undefined);
      clearEditParam(id);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setConflict(true);
      else setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };
  const reloadTheirs = () => {
    setDraft(null);
    setConflict(false);
    setProblems([]);
    clearEditParam(id);
    reload();
  };
  const remove = async () => {
    setDeleteError(undefined);
    try {
      await endpoints.deleteDashboard(id);
      window.location.hash = "#/dashboards";
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) window.location.hash = "#/dashboards";
      else setDeleteError(e instanceof Error ? e.message : String(e));
    }
  };

  const eyebrow = (
    <span className="inline-flex flex-wrap items-center gap-x-3 gap-y-1">
      <a href="#/dashboards" className={control.link}>
        ← Dashboards
      </a>
      <span>{plural(shown.widgets.length, "widget")}</span>
      {b.layout?.autogen && <span className="border border-border px-1.5 text-foreground">auto · written by the supervisor</span>}
      {b.updatedAt && <span>saved {ago(b.updatedAt)}</span>}
    </span>
  );

  return (
    <div className="space-y-10 sm:space-y-14">
      <PageHead eyebrow={eyebrow} lines={[shown.title.trim() || "Untitled board"]} lead={shown.description || undefined} />

      <div className="rise-in flex flex-wrap items-center justify-between gap-x-6 gap-y-3 border-y border-hair py-3" style={stagger(3)}>
        <div className="flex flex-wrap gap-2" role="group" aria-label="Time range">
          {RANGES.map((r) => (
            <button key={r.id} type="button" className={r.id === rangeId ? control.buttonActive : control.button} aria-pressed={r.id === rangeId} onClick={() => setRangeOverride(r.id)}>
              {r.label}
            </button>
          ))}
        </div>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <span className={typography.eyebrow}>refreshes every {refresh < 60 ? `${refresh} s` : `${Math.round(refresh / 60)} min`}</span>
          {draft ? (
            <>
              <button type="button" className={control.button} onClick={cancelEdit} disabled={saving}>
                Cancel
              </button>
              <button type="button" className={control.buttonActive} onClick={save} disabled={saving}>
                {saving ? "Saving…" : "Save"}
              </button>
            </>
          ) : (
            <button type="button" className={control.button} onClick={startEdit}>
              Edit
            </button>
          )}
        </div>
      </div>

      {draft && (
        <Builder
          draft={draft}
          onChange={setDraft}
          problems={problems}
          saveError={saveError}
          conflict={conflict}
          onReload={reloadTheirs}
          confirmDelete={confirmDelete}
          setConfirmDelete={setConfirmDelete}
          deleteError={deleteError}
          onDelete={remove}
        />
      )}

      <BoardGrid board={shown} rangeId={rangeId} refreshMs={refresh * 1000} preview={!!draft} />
    </div>
  );
}

function BackLink() {
  return (
    <a href="#/dashboards" className={control.link}>
      ← All dashboards
    </a>
  );
}

// ---- the grid ----

function BoardGrid({ board, rangeId, refreshMs, preview }: { board: Dashboard; rangeId: string; refreshMs: number; preview: boolean }) {
  // While editing, the preview waits for typing to pause before it queries.
  const settled = useDebounced(board, preview ? 800 : 0);
  const shown = preview ? settled : board;
  const order = useMemo(() => readingOrder(shown.widgets), [shown.widgets]);

  if (!shown.widgets.length) {
    return (
      <Empty>
        {preview ? 'This board has no widgets. Use "Add widget" above.' : 'This board has no widgets yet. Choose "Edit" to add charts, stats, logs or alarms.'}
      </Empty>
    );
  }

  return (
    <div className="space-y-4">
      {preview && <p className={typography.eyebrow}>preview · updates when you pause typing · not saved yet</p>}
      <div className="grid grid-cols-1 gap-x-6 gap-y-10 md:grid-cols-12 md:auto-rows-[minmax(5.5rem,auto)] md:gap-y-6">
        {order.map((i, k) => {
          const w = shown.widgets[i];
          const x = Math.max(0, Math.min(11, w.x));
          const span = Math.max(1, Math.min(12 - x, w.w));
          const place = { "--gc": `${x + 1} / span ${span}`, "--gr": `${Math.max(0, w.y) + 1} / span ${Math.max(1, w.h)}` } as CSSProperties;
          return (
            <WidgetSection key={w.id ?? `new-${i}`} widget={w} index={k + 4} style={place} className="md:[grid-column:var(--gc)] md:[grid-row:var(--gr)]">
              <WidgetBody widget={w} rangeId={rangeId} refreshMs={refreshMs} />
            </WidgetSection>
          );
        })}
      </div>
    </div>
  );
}

// ---- builder ----

function Builder({
  draft,
  onChange,
  problems,
  saveError,
  conflict,
  onReload,
  confirmDelete,
  setConfirmDelete,
  deleteError,
  onDelete,
}: {
  draft: Dashboard;
  onChange: (d: Dashboard) => void;
  problems: string[];
  saveError?: string;
  conflict: boolean;
  onReload: () => void;
  confirmDelete: boolean;
  setConfirmDelete: (v: boolean) => void;
  deleteError?: string;
  onDelete: () => void;
}) {
  const catalog = useSeriesCatalog();
  const series = catalog.data ?? [];
  const services = useMemo(() => [...new Set(series.map((s) => s.dimensions?.Service).filter((s): s is string => !!s && s !== "*"))].sort(), [series]);
  const widgets = draft.widgets;
  const setWidgets = (ws: typeof widgets) => onChange({ ...draft, widgets: ws });

  return (
    <div className="space-y-10">
      {conflict && (
        <div className="rise-in flex flex-wrap items-center justify-between gap-3 border border-tone-warning/55 bg-tone-warning/10 p-4">
          <p className="text-[0.95rem]">Someone else saved this board. Reload to see their version.</p>
          <button type="button" className={control.buttonActive} onClick={onReload}>
            Reload
          </button>
        </div>
      )}
      {saveError && (
        <Empty>
          Could not save: <span className="font-mono text-[0.8rem]">{saveError}</span>.
        </Empty>
      )}
      {problems.length > 0 && (
        <div className="border border-tone-critical/50 bg-tone-critical/10 p-4" role="alert">
          <p className={`${typography.micro} mb-2`}>Not saved · fix {problems.length === 1 ? "this" : `these ${problems.length}`} first</p>
          <ul className="list-inside list-disc space-y-1 text-[0.95rem]">
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      )}

      <Section index={4} title="Board">
        <BoardSettings draft={draft} onChange={onChange} />
      </Section>

      <Section index={5} title="Layout" note="drag a block to move it, its corner to resize · numbers match the widgets below · phones stack by row">
        {widgets.length ? (
          <LayoutMap widgets={widgets} onMove={(i, pos) => setWidgets(widgets.map((w, k) => (k === i ? { ...w, ...pos } : w)))} />
        ) : (
          <p className="text-[0.95rem] text-muted-foreground">No widgets yet.</p>
        )}
      </Section>

      <Section
        index={6}
        title="Widgets"
        note={`${widgets.length} of at most ${MAX_WIDGETS}`}
        action={
          <button type="button" className={control.buttonActive} disabled={widgets.length >= MAX_WIDGETS} onClick={() => setWidgets([...widgets, newWidget(widgets, series)])}>
            Add widget
          </button>
        }
      >
        {catalog.error && !catalog.data && <ErrorNote error={catalog.error} what="the metric catalog" />}
        {widgets.length === 0 ? (
          <p className="text-[0.95rem] text-muted-foreground">Add a widget to start. Each one is a time series until you change its type.</p>
        ) : (
          <div className="space-y-6">
            {widgets.map((w, i) => (
              <WidgetEditor
                key={w.id ?? `new-${i}`}
                widget={w}
                index={i}
                count={widgets.length}
                catalog={series}
                services={services}
                onChange={(nw) => setWidgets(widgets.map((x, k) => (k === i ? nw : x)))}
                onRemove={() => setWidgets(widgets.filter((_, k) => k !== i))}
                onMove={(dir) => {
                  const j = i + dir;
                  if (j < 0 || j >= widgets.length) return;
                  const next = [...widgets];
                  [next[i], next[j]] = [next[j], next[i]];
                  setWidgets(next);
                }}
              />
            ))}
          </div>
        )}
      </Section>

      <Section index={7} title="Delete board">
        {confirmDelete ? (
          <div className="flex flex-wrap items-center gap-3 border border-tone-critical/50 bg-tone-critical/10 p-4">
            <p className="mr-auto text-[0.95rem]">Delete “{draft.title || "Untitled board"}” for everyone? This cannot be undone.</p>
            <button type="button" className={control.button} onClick={() => setConfirmDelete(false)}>
              Keep it
            </button>
            <button type="button" className={control.buttonActive} onClick={onDelete}>
              Delete this board
            </button>
          </div>
        ) : (
          <button type="button" className={control.button} onClick={() => setConfirmDelete(true)}>
            Delete board…
          </button>
        )}
        {deleteError && (
          <p className="mt-3 text-[0.95rem]">
            Could not delete: <span className="font-mono text-[0.8rem]">{deleteError}</span>.
          </p>
        )}
      </Section>
    </div>
  );
}
