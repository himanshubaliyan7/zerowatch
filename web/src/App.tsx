import { Component, lazy, Suspense, useEffect, type ReactNode } from "react";
import { useRoute } from "@/lib/hooks";
import { Shell, VIEWS } from "@/components/Shell";
import { Empty, Skeleton } from "@/design/primitives";
import { control } from "@/design/tokens";
import { Overview } from "@/views/Overview";

const RELOADED_KEY = "zerowatch:chunk-reload";

/**
 * Keeps one failing view from blanking the page. A failed lazy import almost
 * always means ZeroWatch was upgraded while this tab was open (the old chunk
 * names are gone), so that case reloads once by itself.
 */
class ViewBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error) {
    if (!/dynamically imported module|Importing a module script failed|Loading chunk/i.test(error.message)) return;
    try {
      if (sessionStorage.getItem(RELOADED_KEY)) return;
      sessionStorage.setItem(RELOADED_KEY, "1");
    } catch {
      return;
    }
    window.location.reload();
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <Empty>
        This view failed to load: <span className="font-mono text-[0.8rem]">{this.state.error.message}</span>. If ZeroWatch was just upgraded, a reload fetches the new version.{" "}
        <button type="button" className={control.link} onClick={() => window.location.reload()}>
          Reload
        </button>
      </Empty>
    );
  }
}

// Everything but the landing view loads on demand, so the first paint stays light.
const ServiceMap = lazy(() => import("@/views/ServiceMap").then((m) => ({ default: m.ServiceMap })));
const Logs = lazy(() => import("@/views/Logs").then((m) => ({ default: m.Logs })));
const Traces = lazy(() => import("@/views/Traces").then((m) => ({ default: m.Traces })));
const Dashboards = lazy(() => import("@/views/Dashboards").then((m) => ({ default: m.Dashboards })));
const Explore = lazy(() => import("@/views/Explore").then((m) => ({ default: m.Explore })));
const Alarms = lazy(() => import("@/views/Alarms").then((m) => ({ default: m.Alarms })));
const Reports = lazy(() => import("@/views/Reports").then((m) => ({ default: m.Reports })));
const Supervisor = lazy(() => import("@/views/Supervisor").then((m) => ({ default: m.Supervisor })));

export function App() {
  const { view, params } = useRoute();
  const known = VIEWS.some((v) => v.id === view) ? view : "overview";

  useEffect(() => {
    // A view loaded fine, so a later chunk failure may reload once again.
    try {
      sessionStorage.removeItem(RELOADED_KEY);
    } catch {
      // Storage blocked: the boundary then never auto-reloads; harmless.
    }
  }, []);

  useEffect(() => {
    const label = VIEWS.find((v) => v.id === known)?.label ?? "Overview";
    document.title = known === "overview" ? "ZeroWatch" : `${label} · ZeroWatch`;
    window.scrollTo(0, 0);
  }, [known]);

  const num = (k: string) => {
    const v = Number(params.get(k));
    return Number.isFinite(v) && v > 0 ? v : undefined;
  };

  let body;
  switch (known) {
    case "map":
      body = <ServiceMap />;
      break;
    case "logs":
      body = <Logs initialQuery={params.get("q") ?? ""} from={num("from")} to={num("to")} />;
      break;
    case "traces":
      body = <Traces id={params.get("id") ?? undefined} service={params.get("service") ?? undefined} />;
      break;
    case "dashboards":
      body = <Dashboards id={params.get("id") ?? undefined} />;
      break;
    case "explore":
      body = <Explore initial={params.get("m") ?? undefined} />;
      break;
    case "alarms":
      body = <Alarms />;
      break;
    case "reports":
      body = <Reports range={params.get("range") ?? undefined} />;
      break;
    case "supervisor":
      body = <Supervisor />;
      break;
    default:
      body = <Overview />;
  }

  return (
    <Shell view={known}>
      {/* key= remounts the view so its entrance motion runs on each visit */}
      <div key={`${known}?${params.toString()}`}>
        <ViewBoundary>
          <Suspense fallback={<Skeleton className="h-64 w-full" />}>{body}</Suspense>
        </ViewBoundary>
      </div>
    </Shell>
  );
}
