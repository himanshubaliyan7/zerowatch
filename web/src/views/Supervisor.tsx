/**
 * What the autonomous supervisor (RedBrain) found and decided: its loop as a
 * diagram with live counts, the host, the decision log, discovered services,
 * tailed log files and containers.
 */
import { endpoints, ApiError, type SupervisorState } from "@/lib/api";
import { usePoll } from "@/lib/hooks";
import { ago, bytes, num, plural } from "@/lib/format";
import { Empty, ErrorNote, PageHead, Section, Skeleton, Stat, StatGrid, StatusMark, stagger } from "@/design/primitives";
import { typography } from "@/design/tokens";

export function Supervisor() {
  const sup = usePoll(() => endpoints.supervisor(), 5_000);

  if (sup.error instanceof ApiError && sup.error.status === 404) {
    return (
      <div className="space-y-12">
        <PageHead eyebrow="Supervisor" lines={["The supervisor is off."]} />
        <Empty hatch>
          This node runs without the autonomous supervisor, so nothing is discovered or synthesized here. Start ZeroWatch with
          <span className="font-mono text-[0.8rem]"> --supervisor=true</span> (on by default for <span className="font-mono text-[0.8rem]">--role=all</span>).
        </Empty>
      </div>
    );
  }
  const s = sup.data;
  return (
    <div className="space-y-12 sm:space-y-16">
      <PageHead
        eyebrow={s ? `Cycle ${num(s.cycle)} · last run ${ago(s.lastRun)} · CPU ${num(s.watchdog.supervisorCpuPct, 2)} % of 1 % budget` : "Supervisor"}
        lines={["What the supervisor did."]}
      />
      {sup.error && !s ? (
        <ErrorNote error={sup.error} what="the supervisor state" />
      ) : !s ? (
        <Skeleton className="h-40 w-full" />
      ) : (
        <Body s={s} />
      )}
    </div>
  );
}

function Body({ s }: { s: SupervisorState }) {
  const services = s.services ?? [];
  const targets = s.targets ?? [];
  const sources = s.logSources ?? [];
  const synthesized = s.synthesized ?? [];
  const containers = s.containers ?? [];
  const decisions = [...(s.decisions ?? [])].reverse();
  const formats = new Set(sources.map((l) => l.guess.format));
  const probeNote = `probe: ${s.probe.mode === "ebpf" ? "eBPF" : "/proc fallback"}${s.probe.error ? ` · ${s.probe.error}` : ""}`;

  const steps = [
    { n: "01 Sense", k: "Processes", v: `${plural(services.length, "service")} listening` },
    { n: "02 Decide", k: "Targets", v: plural(targets.length, "port") },
    { n: "03 Attach", k: "Probe", v: `${s.probe.mode} · rate ${num(s.watchdog.sampleRate, 2)}` },
    { n: "04 Tail", k: "Logs", v: `${plural(sources.length, "file")} · ${plural(formats.size, "format")}` },
    { n: "05 Synthesize", k: "Boards", v: `${plural(synthesized.filter((x) => x.startsWith("dashboard")).length, "board")} · ${plural(synthesized.filter((x) => x.startsWith("alarm")).length, "alarm")}` },
  ];

  return (
    <>
      <Section index={1} title="Loop" note={probeNote}>
        <ol className="flex flex-col md:flex-row md:items-stretch">
          {steps.map((st, i) => (
            <li key={st.n} className="flex flex-col md:flex-1 md:flex-row md:items-center">
              {i > 0 && <span className="flow-y ml-6 h-5 md:hidden" aria-hidden="true" />}
              {i > 0 && <span className="flow-x hidden w-6 md:block" aria-hidden="true" />}
              <div className="min-w-0 flex-1 border border-border p-4">
                <div className={typography.eyebrow}>{st.n}</div>
                <div className="mt-1.5 font-display text-lg font-semibold uppercase">{st.k}</div>
                <div className={`${typography.eyebrow} mt-0.5`}>{st.v}</div>
              </div>
            </li>
          ))}
        </ol>
        <p className={`${typography.eyebrow} mt-3`}>then 06 Self-heal: the watchdog halves the probe's sample rate when the supervisor goes over budget</p>
      </Section>

      <Section index={2} title="Host" note={`${s.host.hostname} · ${s.host.os}/${s.host.arch}${s.host.kernel ? ` · kernel ${s.host.kernel}` : ""}`}>
        <StatGrid>
          <Stat label="CPUs" value={s.host.cpus} unit={s.host.cloud ? `on ${s.host.cloud}` : s.host.inKubernetes ? "in Kubernetes" : s.host.inContainer ? "in a container" : "bare host or VM"} />
          <Stat label="Process RSS" value={s.watchdog.rssMb} decimals={0} unit="MB of 100 MB budget" />
          <Stat label="Probe events" value={s.probe.events} unit={s.probe.healthy ? "healthy" : "not healthy"} />
          <Stat label="Watched ports" value={targets.length} unit="probe targets" />
        </StatGrid>
        <div className="mt-3 flex flex-wrap gap-x-6 gap-y-2">
          <StatusMark tone={s.host.ebpfCapable ? "positive" : "unknown"} label={s.host.ebpfCapable ? "eBPF capable" : "eBPF not available"} />
          <StatusMark tone={s.host.btf ? "positive" : "unknown"} label={s.host.btf ? "BTF present" : "no BTF"} />
          <StatusMark tone={s.probe.healthy ? "positive" : "critical"} label={`probe ${s.probe.healthy ? "healthy" : "unhealthy"}`} />
          <StatusMark tone={s.watchdog.throttled ? "warning" : "positive"} label={s.watchdog.throttled ? "throttled by watchdog" : "within budget"} />
        </div>
      </Section>

      <Section index={3} title="Decision log" note="newest first">
        {decisions.length === 0 ? (
          <Empty>No decisions yet. The first cycle runs a few seconds after start.</Empty>
        ) : (
          <ol className="border-b border-hair">
            {decisions.map((d, i) => {
              const m = /^(\d\d:\d\d:\d\d) (.*)$/.exec(d);
              return (
                <li key={`${i}-${d}`} className="grid grid-cols-[5.5rem_minmax(0,1fr)] gap-4 border-t border-hair py-2.5 text-[0.95rem]">
                  <span className={`${typography.eyebrow} pt-0.5 tabular-nums`}>{m ? m[1] : "—"}</span>
                  <span className="[overflow-wrap:anywhere]">{m ? m[2] : d}</span>
                </li>
              );
            })}
          </ol>
        )}
      </Section>

      <Section index={4} title="Services found" note={`${plural(services.length, "process", "processes")} with a listening port or a known kind`}>
        {services.length === 0 ? (
          <Empty hatch>No listening processes found. On Windows or macOS the /proc scan finds nothing; run ZeroWatch on Linux to discover services.</Empty>
        ) : (
          <div className="relative overflow-x-auto">
            <table className="w-full min-w-[36rem] text-left text-[0.95rem]">
              <thead>
                <tr className={typography.eyebrow}>
                  <th className="pb-2.5 pr-3 font-medium">Process</th>
                  <th className="pb-2.5 pr-3 font-medium">Kind</th>
                  <th className="pb-2.5 pr-3 text-right font-medium">PID</th>
                  <th className="pb-2.5 pr-3 font-medium">Ports</th>
                  <th className="pb-2.5 text-right font-medium">RSS</th>
                </tr>
              </thead>
              <tbody>
                {services.map((p) => (
                  <tr key={p.pid} className="border-t border-hair">
                    <td className="py-2.5 pr-3 font-display font-semibold uppercase">{p.comm}</td>
                    <td className="py-2.5 pr-3 text-muted-foreground">{p.kind || "—"}</td>
                    <td className="py-2.5 pr-3 text-right tabular-nums">{p.pid}</td>
                    <td className="py-2.5 pr-3 font-mono text-[0.8rem]">{p.ports?.length ? p.ports.join(" ") : "—"}</td>
                    <td className="py-2.5 text-right tabular-nums">{bytes(p.rssKb * 1024)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>

      <Section index={5} title="Log files" note="format inferred from a 20-line sample">
        {sources.length === 0 ? (
          <Empty hatch>No log files tailed. Log discovery scans the roots in --log-roots (default /var/log).</Empty>
        ) : (
          <ul className="border-b border-hair">
            {sources.map((l, i) => (
              <li key={l.path} className="rise-in grid gap-x-6 gap-y-1 border-t border-hair py-2.5 sm:grid-cols-[minmax(0,1fr)_auto_auto]" style={stagger(i)}>
                <span className="font-mono text-[0.8rem] [overflow-wrap:anywhere]">{l.path}</span>
                <span className={typography.eyebrow}>
                  {l.guess.format} · confidence {num(l.guess.confidence, 2)}
                </span>
                <span className={`${typography.eyebrow} sm:text-right`}>
                  {num(l.shipped)} lines shipped{l.error ? ` · ${l.error}` : ""}
                </span>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section index={6} title="Containers" note={s.host.dockerSocket ? `docker socket ${s.host.dockerSocket}` : "no docker socket"}>
        {containers.length === 0 ? (
          <Empty hatch>
            {s.host.dockerSocket ? "The Docker socket answered with no running containers." : "No Docker socket found on this host, so no containers were listed. Nothing is wrong; there is just nothing to show."}
          </Empty>
        ) : (
          <ul className="border-b border-hair">
            {containers.map((c) => (
              <li key={c.id} className="grid gap-x-6 border-t border-hair py-2.5 sm:grid-cols-[minmax(0,1fr)_auto_auto]">
                <span className="font-display font-semibold uppercase [overflow-wrap:anywhere]">{c.name}</span>
                <span className="font-mono text-[0.8rem] text-muted-foreground [overflow-wrap:anywhere]">{c.image}</span>
                <StatusMark tone={c.state === "running" ? "positive" : "unknown"} label={c.state} />
              </li>
            ))}
          </ul>
        )}
      </Section>
    </>
  );
}
