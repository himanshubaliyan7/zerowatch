// Command zerowatch runs the ZeroWatch observability platform. With
// --role=all it is fully self-contained: embedded storage, SQLite metadata,
// in-memory commit log, synthetic RED/log generator, host supervisor and UI.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/himanshubaliyan7/zerowatch/ebpf/probe"
	"github.com/himanshubaliyan7/zerowatch/internal/alarms"
	"github.com/himanshubaliyan7/zerowatch/internal/api"
	"github.com/himanshubaliyan7/zerowatch/internal/authn"
	"github.com/himanshubaliyan7/zerowatch/internal/awssig"
	"github.com/himanshubaliyan7/zerowatch/internal/cluster"
	"github.com/himanshubaliyan7/zerowatch/internal/commitlog"
	"github.com/himanshubaliyan7/zerowatch/internal/config"
	"github.com/himanshubaliyan7/zerowatch/internal/forecast"
	"github.com/himanshubaliyan7/zerowatch/internal/generator"
	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
	"github.com/himanshubaliyan7/zerowatch/internal/ingest"
	"github.com/himanshubaliyan7/zerowatch/internal/leader"
	"github.com/himanshubaliyan7/zerowatch/internal/live"
	"github.com/himanshubaliyan7/zerowatch/internal/metadata"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/promapi"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
	"github.com/himanshubaliyan7/zerowatch/internal/reports"
	"github.com/himanshubaliyan7/zerowatch/internal/storage"
	"github.com/himanshubaliyan7/zerowatch/internal/supervisor"
	"github.com/himanshubaliyan7/zerowatch/internal/tier"
	"github.com/himanshubaliyan7/zerowatch/internal/traces"
	"github.com/himanshubaliyan7/zerowatch/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "zerowatch:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: lvl}
	var log *slog.Logger
	if cfg.LogJSON {
		log = slog.New(slog.NewJSONHandler(os.Stderr, opts))
	} else {
		log = slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}

	var store storage.Engine
	var tr *tier.Tier
	if cfg.StorageBackend == "clickhouse" {
		ch := storage.NewClickHouse(cfg.ClickHouseURL, cfg.ClickHouseDB, os.Getenv("ZW_CLICKHOUSE_USER"), os.Getenv("ZW_CLICKHOUSE_PASSWORD"))
		if err := ch.EnsureSchema(ctx); err != nil {
			return fmt.Errorf("clickhouse schema: %w", err)
		}
		store = ch
	} else {
		memCfg := storage.MemoryConfig{RawRetention: cfg.RawRetention, RollupRetention: cfg.RollupRetention, MaxSeries: cfg.MaxSeries, MaxLogEvents: cfg.MaxLogEvents}
		// Cold tier and per-tenant retention: with either set, the policy's
		// cutoffs replace --raw-retention/--rollup-retention.
		if cfg.TierBucket != "" || cfg.Retention != "" || len(cfg.RetentionTenant) > 0 {
			ret, err := tier.ParseRetention(cfg.Retention, cfg.RetentionTenant)
			if err != nil {
				return fmt.Errorf("retention: %w", err)
			}
			if ret.Default.RawHot == 0 {
				ret.Default.RawHot = cfg.TierHot
			}
			tc := tier.Config{Prefix: cfg.TierPrefix, Retention: ret, Logger: log}
			if cfg.TierBucket != "" {
				s3, err := tier.NewS3(tier.S3Config{Endpoint: cfg.TierS3Endpoint, Bucket: cfg.TierBucket, Region: cfg.TierRegion, PathStyle: cfg.TierPathStyle, Creds: awssig.FromEnv("ZW_TIER_", "AWS_")})
				if err != nil {
					return fmt.Errorf("tier: %w", err)
				}
				if err := s3.EnsureBucket(ctx); err != nil {
					log.Warn("tier: bucket check", "err", err)
				}
				tc.Store = s3
			}
			tr = tier.New(tc)
			if err := tr.Load(ctx); err != nil {
				return fmt.Errorf("tier: index: %w", err)
			}
			tr.Start()
			memCfg.Tier = tr
			// Runs after store.Close (defers are LIFO): flush what's queued.
			defer func() {
				sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := tr.Close(sctx); err != nil {
					log.Warn("tier: final flush", "err", err)
				}
			}()
		}
		store = storage.NewMemory(memCfg)
	}
	defer store.Close()

	// Cluster: standalone (no gossip, no internal RPC) unless seeds are set or
	// the role is split; then store becomes the ring-routing engine.
	// Spans: each trace lives on its ring owner in cluster mode (node.Traces
	// routes them), in this process otherwise.
	localTraces := traces.NewStore(traces.Config{})
	node, err := cluster.Start(cluster.Options{NodeName: cfg.NodeName, Role: cfg.Role, HTTPAddr: cfg.HTTPAddr,
		GossipPort: cfg.GossipPort, Seeds: cfg.ClusterSeeds, Secret: os.Getenv("ZW_CLUSTER_SECRET")}, store, localTraces, log)
	if err != nil {
		return err
	}
	defer node.Close()
	store = node.Store

	meta, err := metadata.Open(ctx, cfg.MetadataDSN)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	defer meta.Close()

	hub := live.NewHub(256, 1000)

	// Commit log: ingest -> log -> storage writer (+ live fan-out).
	var producer commitlog.Producer
	var consumer commitlog.Consumer
	var memLog *commitlog.Memory
	var kafkaLog *commitlog.Kafka
	if cfg.CommitLog == "kafka" {
		// Only nodes that hold data consume; ingest/query/supervisor nodes just
		// publish, otherwise they would take partitions and write elsewhere.
		consumes := cfg.Role == config.RoleAll || cfg.Role == config.RoleStorage
		k, err := commitlog.NewKafkaConfig(commitlog.KafkaConfig{Brokers: cfg.KafkaBrokers, Topic: cfg.KafkaTopic, Log: log, ProduceOnly: !consumes})
		if err != nil {
			return err
		}
		producer, kafkaLog = k, k
		if consumes {
			consumer = k
		}
	} else {
		// The in-memory log only exists in this process, so its consumer
		// always runs here (cluster mode then routes writes via node.Store).
		memLog = commitlog.NewMemory(65536)
		producer, consumer = memLog, memLog
	}
	defer producer.Close()
	go func() {
		if consumer == nil {
			return
		}
		handle := func(ctx context.Context, recs []commitlog.Record) error {
			var ms []model.MetricDatum
			var ls []model.LogEvent
			for _, r := range recs {
				if r.Metric != nil {
					ms = append(ms, *r.Metric)
				}
				if r.Log != nil {
					ls = append(ls, *r.Log)
				}
			}
			if len(ms) > 0 {
				if err := store.WriteMetrics(ctx, ms); err != nil && !errors.Is(err, storage.ErrCardinality) {
					return err
				}
			}
			if len(ls) > 0 {
				if err := store.WriteLogs(ctx, ls); err != nil {
					return err
				}
				hub.PublishLogs(ls)
			}
			return nil
		}
		// Cluster mode: a data-owning node forwards each consumed record to
		// its ring owner (failover to the replica, else kept here) instead of
		// storing whatever partition it was given.
		if node.Consume != nil {
			node.Consume.OnLogs = hub.PublishLogs
			handle = node.Consume.Handle
			if kafkaLog != nil {
				kafkaLog.SetRouteStats(node.Consume.Stats)
			}
			if memLog != nil {
				memLog.SetRouteStats(node.Consume.Stats)
			}
		}
		err := consumer.Run(ctx, handle)
		if err != nil && ctx.Err() == nil {
			log.Error("commit log consumer stopped", "err", err)
		}
	}()

	pipe := ingest.NewPipeline(producer, log)
	tstore := node.Traces
	qe := &query.Engine{Fetcher: store, MaxParallel: 8}
	fc := &forecast.Service{Query: qe, ModelURL: cfg.ForecasterURL, Client: &http.Client{Timeout: 20 * time.Second}}
	if cfg.ForecasterURL != "" {
		log.Info("model forecaster configured, built-in methods are the fallback", "url", cfg.ForecasterURL)
	}
	alarmEngine := &alarms.Engine{Rules: meta, Sink: meta, Query: qe, Dispatcher: alarms.NewDispatcher(log), Log: log, Forecaster: fc,
		IngestLag: 30 * time.Second, OnTransition: func(tr alarms.Transition) {
			hub.Publish(live.Event{Kind: live.KindAlarm, TenantID: 1, Time: time.Now().UnixMilli(), Data: tr})
		}}

	if cfg.Generate {
		g := generator.New(generator.Config{Services: cfg.GenerateServices, Tenant: 1, Traces: tstore}, pipe, log)
		go g.Run(ctx)
		log.Info("synthetic RED + log generator enabled", "services", g.Services())
	}
	var sup *supervisor.Supervisor
	if cfg.Supervisor {
		sup = supervisor.New(supervisor.Config{Interval: cfg.SupervisorEvery, LogDiscovery: cfg.LogDiscovery, LogRoots: cfg.LogRoots, Tenant: 1,
			HostMetrics: cfg.HostMetrics, HostFSRoot: cfg.HostRoot, HostName: cfg.HostName},
			probe.New(log, cfg.BPFObject), pipe, meta, store, hub, log)
		go sup.Run(ctx)
	}

	static, _ := fs.Sub(web.Dist, "dist")
	reportTargets, err := reports.ParseTargets(cfg.ReportTargets)
	if err != nil {
		return err
	}
	rb := &reports.Builder{Series: store, Query: qe, Alarms: meta, States: alarmEngine.States, Traces: tstore, Forecast: fc,
		Targets: reportTargets, Log: log}
	scheduledReports := cfg.ReportEvery > 0 && len(reportTargets) > 0
	if cfg.ReportEvery > 0 && len(reportTargets) == 0 {
		log.Warn("report-every is set but no report-targets; scheduled reports disabled")
	}
	// Alarms and scheduled reports run on exactly one query replica: the
	// holder of the "singletons" lease in the metadata DB. Replicas with their
	// own SQLite each win their own lease, so HA needs a shared Postgres.
	var elector *leader.Elector
	electorDone := make(chan struct{})
	if cfg.Runs(config.RoleQuery) {
		if node.Engine != nil && strings.HasPrefix(cfg.MetadataDSN, "sqlite:") {
			log.Warn("cluster mode with SQLite metadata: every query replica will send alarm notifications; set --metadata-dsn to a shared Postgres")
		}
		elector = &leader.Elector{Leases: meta, Name: "singletons", Holder: leader.NewHolder(cfg.NodeName), TTL: cfg.LeaderLease, Log: log}
		go func() {
			defer close(electorDone)
			elector.Run(ctx, func(lctx context.Context) {
				var wg sync.WaitGroup
				wg.Add(1)
				go func() { defer wg.Done(); alarmEngine.Run(lctx, 10*time.Second) }()
				if scheduledReports {
					wg.Add(1)
					go func() { defer wg.Done(); rb.Run(lctx, cfg.ReportEvery, 1, cfg.ReportRange) }()
				}
				wg.Wait()
				alarmEngine.Reset()
			})
		}()
	} else {
		close(electorDone)
	}
	srv := &api.Server{Storage: store, Meta: meta, Query: qe, Pipeline: pipe, Hub: hub, Alarms: alarmEngine, Static: static, Log: log, Traces: tstore, Forecast: fc,
		Extra: func() map[string]any {
			out := map[string]any{"role": cfg.Role, "node": cfg.NodeName, "ebpfBuild": probe.Available}
			if elector != nil {
				out["leader"] = elector.Leading()
			}
			if memLog != nil {
				out["commitLog"] = memLog.Stats()
			}
			if kafkaLog != nil {
				out["commitLog"] = kafkaLog.Stats()
			}
			if tr != nil {
				out["tier"] = tr.Stats()
			}
			if sup != nil {
				out["supervisor"] = sup.State()
			}
			return out
		}}
	mux := srv.Routes()
	mux.HandleFunc("GET /api/v1/cluster", node.StatusHandler)
	rb.Register(mux)
	if cfg.PrometheusAPI {
		// Grafana's built-in Prometheus datasource: URL http://host:8080/prometheus,
		// API key as a custom "Authorization: Bearer <key>" header.
		(&promapi.Handler{Store: store, Log: log}).Register(mux, "/prometheus")
	}
	if sup != nil {
		mux.HandleFunc("GET /api/v1/supervisor", func(w http.ResponseWriter, r *http.Request) { httpx.WriteJSON(w, 200, sup.State()) })
	}
	// OIDC browser sign-in (optional). /api/v1/auth/me is always mounted;
	// /auth/{login,callback,logout} only when OIDC is on.
	var oidc *authn.Authenticator
	if cfg.OIDCIssuer != "" {
		oidc, err = authn.New(authn.Config{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL: cfg.OIDCRedirectURL, RoleClaim: cfg.OIDCRoleClaim, RoleMap: cfg.OIDCRoleMap, DefaultRole: cfg.OIDCDefaultRole,
			SessionKey: []byte(cfg.SessionKey), CookieSecure: strings.HasPrefix(cfg.OIDCRedirectURL, "https://"), Log: log})
		if err != nil {
			return err
		}
		if err := oidc.Discover(ctx); err != nil {
			log.Warn("OIDC provider not reachable yet; sign-in retries on demand", "err", err)
		}
		log.Info("OIDC sign-in enabled", "issuer", cfg.OIDCIssuer, "roleClaim", cfg.OIDCRoleClaim)
	}
	authn.Register(mux, oidc)

	ks := httpx.NewKeyStore(nil)
	for _, k := range cfg.APIKeys {
		role, _ := httpx.ParseRole(k.Role) // "" -> admin
		ks.AddKey(k.Key, k.TenantID, role)
	}
	awsCreds := make([]httpx.AWSCredential, 0, len(cfg.AWSCredentials))
	for _, c := range cfg.AWSCredentials {
		awsCreds = append(awsCreds, httpx.AWSCredential{AccessKeyID: c.AccessKeyID, Secret: c.Secret, Tenant: c.TenantID})
	}
	ks.SetAWSCredentials(awsCreds)
	var sess httpx.Sessions // must stay a nil interface when OIDC is off
	if oidc != nil {
		sess = oidc
	}
	if oidc == nil || ks.Enabled() {
		httpx.LogAuthMode(log, ks)
	}
	rl := httpx.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
	handler := httpx.Chain(mux, httpx.Observe(log), httpx.SecurityHeaders, httpx.CORS(cfg.CORSOrigins),
		rl.Middleware(func(r *http.Request) bool {
			return r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/assets/")
		}),
		httpx.AuthWithSessions(ks, sess, func(r *http.Request) bool { return api.IsPublic(r) || authn.IsPublic(r) }),
		authn.RBAC)
	if node.Internal != nil {
		// Peer RPC bypasses API keys and per-client rate limits; it has its
		// own cluster auth (shared secret or member IP).
		top := http.NewServeMux()
		top.Handle("/internal/", node.Internal)
		top.Handle("/", handler)
		handler = top
	}

	servers := []*http.Server{{Addr: cfg.HTTPAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}}
	if cfg.Runs(config.RoleIngest) {
		for _, a := range []string{cfg.OTLPAddr, cfg.PromAddr} {
			if a != "" && a != cfg.HTTPAddr {
				servers = append(servers, &http.Server{Addr: a, Handler: handler, ReadHeaderTimeout: 10 * time.Second})
			}
		}
		if cfg.StatsDAddr != "" {
			sd := &ingest.StatsDServer{Addr: cfg.StatsDAddr, Pipeline: pipe, Tenant: 1, Interval: 10 * time.Second, Log: log}
			go func() {
				if err := sd.Run(ctx); err != nil && ctx.Err() == nil {
					log.Warn("statsd listener stopped", "err", err)
				}
			}()
		}
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", s.Addr, err)
			}
		}(s)
	}
	log.Info("zerowatch started", "role", cfg.Role, "storage", cfg.StorageBackend, "data", filepath.Clean(cfg.DataDir), "ebpfBuild", probe.Available)
	select {
	case <-ctx.Done():
	case err := <-errc:
		stop()
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	// Let the elector release its lease (a standby takes over at once) before
	// the deferred meta.Close runs.
	select {
	case <-electorDone:
	case <-sctx.Done():
	}
	log.Info("shutdown complete")
	return nil
}
