/**
 * The page frame: sticky header with the brand and the views, the status
 * strip fixed to the foot of the window, and the sign-in (OIDC) or API-key
 * prompt shown when the server has auth on.
 */
import { useEffect, useRef, useState, type ReactNode } from "react";
import { endpoints, fetchMe, getApiKey, onUnauthorized, setApiKey, signInUrl, SIGN_OUT_URL, type Me, type Stats } from "@/lib/api";
import { onLiveStatus, reconnectLive, subscribeLive } from "@/lib/live";
import { useNow, usePoll } from "@/lib/hooks";
import { ago, num } from "@/lib/format";
import { currentTheme, setTheme, type Theme } from "@/lib/theme";
import { control, shell, typography } from "@/design/tokens";

export const VIEWS = [
  { id: "overview", label: "Overview" },
  { id: "map", label: "Map" },
  { id: "logs", label: "Logs" },
  { id: "traces", label: "Traces" },
  { id: "dashboards", label: "Dashboards" },
  { id: "explore", label: "Explore" },
  { id: "alarms", label: "Alarms" },
  { id: "reports", label: "Reports" },
  { id: "supervisor", label: "Supervisor" },
] as const;

export function Shell({ view, children }: { view: string; children: ReactNode }) {
  const stats = usePoll(() => endpoints.stats(), 5_000);
  const [needKey, setNeedKey] = useState(false);
  const [me, setMe] = useState<Me | null>(null);
  useEffect(() => onUnauthorized(() => setNeedKey(true)), []);
  useEffect(() => {
    void fetchMe().then((m) => {
      setMe(m);
      if (m?.oidc && !m.authenticated) setNeedKey(true);
    });
  }, []);

  return (
    <div className={shell.page}>
      <a href="#main" className={`${control.button} sr-only fixed left-2 top-2 z-50 bg-background focus:not-sr-only`}>
        Skip to content
      </a>
      <Header view={view} node={stats.data?.node} me={me} />
      <main id="main" className={shell.main}>
        {needKey && (me?.oidc ? <SignInPrompt /> : <KeyPrompt onDone={() => setNeedKey(false)} />)}
        {children}
      </main>
      <StatusStrip stats={stats.data} error={stats.error} />
    </div>
  );
}

function Header({ view, node, me }: { view: string; node?: string; me: Me | null }) {
  const [theme, setT] = useState<Theme>(currentTheme);
  return (
    <header className={shell.header}>
      <div className={shell.headerInner}>
        <a href="#/" className="flex min-w-0 items-baseline gap-4 py-1">
          <span className={shell.brandName}>ZeroWatch</span>
          <span className={shell.brandTagline}>zero-touch observability{node ? ` · node ${node}` : ""}</span>
        </a>
        <nav className={shell.nav} aria-label="Views">
          {VIEWS.map((v) => (
            <a key={v.id} href={`#/${v.id === "overview" ? "" : v.id}`} className={`${shell.navLink} ${view === v.id ? shell.navLinkActive : ""}`} aria-current={view === v.id ? "page" : undefined}>
              {v.label}
            </a>
          ))}
        </nav>
        <div className="ml-auto flex min-w-0 items-center gap-2 sm:ml-0">
          {me?.method === "oidc" && (
            <>
              <span className={`${typography.eyebrow} mr-2 hidden max-w-[18rem] truncate 2xl:inline`}>
                {me.user} · {me.role}
              </span>
              <a href={SIGN_OUT_URL} className={control.button} title={`Signed in as ${me.user} (${me.role})`}>
                Sign out
              </a>
            </>
          )}
          <button
            type="button"
            className={control.button}
            onClick={() => {
              const next = theme === "dark" ? "light" : "dark";
              setTheme(next);
              setT(next);
            }}
            aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}
          >
            {theme === "dark" ? "Light" : "Dark"}
          </button>
        </div>
      </div>
    </header>
  );
}

function StatusStrip({ stats, error }: { stats?: Stats; error?: Error }) {
  const now = useNow(1000);
  const [live, setLive] = useState(false);
  useEffect(() => onLiveStatus(setLive), []);
  // The strip says what is live, so it keeps the one shared stream open on
  // every view (views that read events subscribe on top of it).
  useEffect(() => subscribeLive(() => {}), []);
  const last = useRef<{ t: number; n: number } | null>(null);
  const [rate, setRate] = useState<number | null>(null);
  const [fresh, setFresh] = useState(0);

  useEffect(() => {
    if (!stats) return;
    const n = stats.ingest.metricsAccepted + stats.ingest.logsAccepted;
    const t = Date.now();
    if (last.current && t > last.current.t) setRate(Math.max(0, (n - last.current.n) / ((t - last.current.t) / 1000)));
    last.current = { t, n };
    setFresh(t);
  }, [stats]);

  return (
    <div className={shell.status} role="status">
      <div className={shell.statusInner}>
        <span className="text-foreground">
          <span className={`${shell.liveDot} ${live ? "blink bg-foreground" : "border border-dashed border-foreground"}`} aria-hidden="true" />
          {live ? "Live" : "Reconnecting"}
        </span>
        {error && !stats ? (
          <span>API unreachable · {error.message}</span>
        ) : (
          <>
            <span>ingest {rate == null ? "—" : num(rate, rate < 10 ? 1 : 0)} rec/s</span>
            {stats && (
              <span>
                storage {stats.storage.backend} · {num(stats.storage.series)} series
              </span>
            )}
            {stats && <span className="hidden sm:inline">probe {stats.ebpfBuild ? "eBPF build" : "/proc fallback"}</span>}
            <span>updated {fresh ? ago(fresh, now) : "—"}</span>
          </>
        )}
      </div>
    </div>
  );
}

function SignInPrompt() {
  return (
    <div className="mb-12 border border-border p-5">
      <p className={typography.micro}>Sign-in required</p>
      <p className="mt-2 max-w-[60ch] text-[0.95rem] leading-relaxed">
        This node signs people in through your identity provider. Your role there decides what you can change here.
      </p>
      <div className="mt-4">
        <a href={signInUrl()} className={control.buttonActive}>
          Sign in
        </a>
      </div>
    </div>
  );
}

function KeyPrompt({ onDone }: { onDone: () => void }) {
  const [key, setKey] = useState(getApiKey());
  return (
    <form
      className="mb-12 border border-border p-5"
      onSubmit={(e) => {
        e.preventDefault();
        setApiKey(key.trim());
        reconnectLive();
        onDone();
        window.location.reload();
      }}
    >
      <p className={typography.micro}>API key required</p>
      <p className="mt-2 max-w-[60ch] text-[0.95rem] leading-relaxed">
        This node has API keys turned on. Paste a key from <span className="font-mono text-[0.8rem]">--api-keys</span>; it is kept in this browser only.
      </p>
      <div className="mt-4 flex flex-wrap gap-2">
        <input className={`${control.input} flex-[1_1_18rem]`} value={key} onChange={(e) => setKey(e.target.value)} aria-label="API key" autoComplete="off" spellCheck={false} />
        <button type="submit" className={control.buttonActive}>
          Use key
        </button>
      </div>
    </form>
  );
}
