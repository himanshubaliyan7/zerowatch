// Package config loads ZeroWatch configuration from flags with environment
// variable overrides (ZW_* names). Flags win over env, env wins over defaults.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Roles a process can run.
const (
	RoleAll        = "all"
	RoleIngest     = "ingest"
	RoleQuery      = "query"
	RoleStorage    = "storage"
	RoleSupervisor = "supervisor"
)

// APIKey maps a secret key to a tenant.
type APIKey struct {
	Key      string
	TenantID uint32
	Role     string // viewer|editor|admin
}

// AWSCredential is a SigV4 access key id / secret pair mapped to a tenant.
// String redacts the secret so printing a Config never leaks it.
type AWSCredential struct {
	AccessKeyID string
	Secret      string
	TenantID    uint32
}

func (c AWSCredential) String() string { return c.AccessKeyID + ":<redacted>" }

// Config is the full process configuration.
type Config struct {
	Role     string
	NodeName string
	LogLevel string
	LogJSON  bool

	HTTPAddr   string // UI + JSON API + CloudWatch-compatible endpoint
	OTLPAddr   string // OTLP/HTTP
	PromAddr   string // Prometheus remote write
	StatsDAddr string // StatsD UDP

	DataDir        string
	MetadataDSN    string // sqlite:<path> | postgres://...
	StorageBackend string // memory | clickhouse
	ClickHouseURL  string
	ClickHouseDB   string

	CommitLog    string // memory | kafka
	KafkaBrokers []string
	KafkaTopic   string

	APIKeys        []APIKey
	AWSCredentials []AWSCredential // SigV4 verification for the CloudWatch endpoints
	RateLimitRPS   float64
	RateLimitBurst int
	CORSOrigins    []string

	// OIDC browser sign-in. The secret and session key come from the
	// environment only (flags show up in ps).
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string // ZW_OIDC_CLIENT_SECRET
	OIDCRedirectURL  string
	OIDCRoleClaim    string
	OIDCRoleMap      map[string]string // claim value -> viewer|editor|admin
	OIDCDefaultRole  string
	SessionKey       string // ZW_SESSION_KEY

	Generate         bool
	GenerateServices int
	Supervisor       bool
	SupervisorEvery  time.Duration
	BPFObject        string // compiled red_probe.bpf.o (used by -tags ebpf builds)
	LogDiscovery     bool   // supervisor tails discovered host log files
	LogRoots         []string
	HostMetrics      bool   // supervisor samples this node's CPU, memory, disks and network
	HostRoot         string // where the host's filesystems are mounted when running in a container
	HostName         string // the supervisor's Host dimension; empty = os.Hostname()
	PrometheusAPI    bool   // serve the Prometheus-compatible query API at /prometheus/api/v1

	// Cold tier (S3/MinIO) and per-tenant retention. Credentials come only
	// from ZW_TIER_ACCESS_KEY_ID/SECRET_ACCESS_KEY/SESSION_TOKEN or AWS_*.
	TierS3Endpoint  string
	TierBucket      string
	TierPrefix      string
	TierRegion      string
	TierPathStyle   bool
	TierHot         time.Duration
	Retention       string
	RetentionTenant []string

	RawRetention    time.Duration
	RollupRetention time.Duration
	MaxSeries       int
	MaxLogEvents    int

	GossipPort   int
	ClusterSeeds []string

	ForecasterURL string        // optional model forecaster service (forecaster/); empty = built-in only
	ReportTargets []string      // "slack:<url>", "webhook:<url>" or "log"
	ReportEvery   time.Duration // scheduled report interval; 0 disables
	ReportRange   string        // range of scheduled reports: 24h|7d|30d

	ShutdownTimeout time.Duration
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseAPIKeys parses "key1:1,key2:2" (tenant defaults to 1 when omitted).
func ParseAPIKeys(s string) ([]APIKey, error) {
	var out []APIKey
	for _, p := range splitList(s) {
		parts := strings.Split(p, ":")
		if len(parts) > 3 {
			return nil, fmt.Errorf("invalid api key entry %q: want KEY[:tenant[:role]]", p)
		}
		tenant, role := uint64(1), "admin"
		if len(parts) >= 2 {
			var err error
			if tenant, err = strconv.ParseUint(parts[1], 10, 32); err != nil || tenant == 0 {
				return nil, fmt.Errorf("invalid tenant in api key entry %q", p)
			}
		}
		if len(parts) == 3 {
			if role = strings.ToLower(strings.TrimSpace(parts[2])); !validRole(role) {
				return nil, fmt.Errorf("invalid role in api key entry %q: want viewer, editor or admin", p)
			}
		}
		if len(parts[0]) < 16 {
			return nil, errors.New("api keys must be at least 16 characters")
		}
		out = append(out, APIKey{Key: parts[0], TenantID: uint32(tenant), Role: role})
	}
	return out, nil
}

func validRole(r string) bool { return r == "viewer" || r == "editor" || r == "admin" }

// ParseRoleMap parses "claimValue=role,...".
func ParseRoleMap(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range splitList(s) {
		k, v, ok := strings.Cut(p, "=")
		k, v = strings.TrimSpace(k), strings.ToLower(strings.TrimSpace(v))
		if !ok || k == "" || !validRole(v) {
			return nil, fmt.Errorf("oidc-role-map entry %q: want claimValue=viewer|editor|admin", p)
		}
		out[k] = v
	}
	return out, nil
}

// ParseAWSCredentials parses "AKID:SECRET:tenant,..." (tenant defaults to 1
// when omitted). Errors name the entry by position, never by content, so a
// typo can't print a secret.
func ParseAWSCredentials(s string) ([]AWSCredential, error) {
	var out []AWSCredential
	seen := map[string]bool{}
	for i, p := range splitList(s) {
		parts := strings.Split(p, ":")
		if len(parts) != 2 && len(parts) != 3 {
			return nil, fmt.Errorf("aws credential entry %d: want ACCESSKEYID:SECRET:tenant", i+1)
		}
		id, secret := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		tenant := uint64(1)
		if len(parts) == 3 {
			var err error
			if tenant, err = strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 32); err != nil || tenant == 0 {
				return nil, fmt.Errorf("aws credential entry %d: invalid tenant", i+1)
			}
		}
		if id == "" || strings.ContainsAny(id, "/ ") {
			return nil, fmt.Errorf("aws credential entry %d: invalid access key id", i+1)
		}
		if len(secret) < 16 {
			return nil, fmt.Errorf("aws credential entry %d: secret must be at least 16 characters", i+1)
		}
		if seen[id] {
			return nil, fmt.Errorf("aws credential entry %d: duplicate access key id %q", i+1, id)
		}
		seen[id] = true
		out = append(out, AWSCredential{AccessKeyID: id, Secret: secret, TenantID: uint32(tenant)})
	}
	return out, nil
}

// Load parses args (without program name) using env defaults.
func Load(args []string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("zerowatch", flag.ContinueOnError)
	host, _ := os.Hostname()
	fs.StringVar(&c.Role, "role", env("ZW_ROLE", RoleAll), "process role: all|ingest|query|storage|supervisor")
	fs.StringVar(&c.NodeName, "node-name", env("ZW_NODE_NAME", host), "cluster node name")
	fs.StringVar(&c.LogLevel, "log-level", env("ZW_LOG_LEVEL", "info"), "debug|info|warn|error")
	fs.BoolVar(&c.LogJSON, "log-json", envBool("ZW_LOG_JSON", false), "emit JSON logs")
	fs.StringVar(&c.HTTPAddr, "http-addr", env("ZW_HTTP_ADDR", ":8080"), "UI/API listen address")
	fs.StringVar(&c.OTLPAddr, "otlp-addr", env("ZW_OTLP_ADDR", ":4318"), "OTLP/HTTP listen address (empty disables)")
	fs.StringVar(&c.PromAddr, "prom-addr", env("ZW_PROM_ADDR", ":8428"), "Prometheus remote-write listen address (empty disables)")
	fs.StringVar(&c.StatsDAddr, "statsd-addr", env("ZW_STATSD_ADDR", ":8125"), "StatsD UDP listen address (empty disables)")
	fs.StringVar(&c.DataDir, "data-dir", env("ZW_DATA_DIR", "./data"), "local data directory")
	fs.StringVar(&c.MetadataDSN, "metadata-dsn", env("ZW_METADATA_DSN", ""), "sqlite:<path> or postgres://... (default sqlite in data-dir)")
	fs.StringVar(&c.StorageBackend, "storage", env("ZW_STORAGE", "memory"), "storage backend: memory|clickhouse")
	fs.StringVar(&c.ClickHouseURL, "clickhouse-url", env("ZW_CLICKHOUSE_URL", "http://localhost:8123"), "ClickHouse HTTP URL")
	fs.StringVar(&c.ClickHouseDB, "clickhouse-db", env("ZW_CLICKHOUSE_DB", "zerowatch"), "ClickHouse database")
	fs.StringVar(&c.CommitLog, "commitlog", env("ZW_COMMITLOG", "memory"), "commit log: memory|kafka")
	brokers := fs.String("kafka-brokers", env("ZW_KAFKA_BROKERS", "localhost:9092"), "comma-separated Kafka/Redpanda brokers")
	fs.StringVar(&c.KafkaTopic, "kafka-topic", env("ZW_KAFKA_TOPIC", "zerowatch-ingest"), "commit log topic")
	keys := fs.String("api-keys", env("ZW_API_KEYS", ""), "comma-separated key:tenant pairs; empty = auth disabled (dev only)")
	awsCreds := fs.String("aws-credentials", env("ZW_AWS_CREDENTIALS", ""), "comma-separated ACCESSKEYID:SECRET:tenant triples; enables SigV4 signature verification (prefer the env var: flags show up in ps)")
	rps := fs.Float64("rate-limit-rps", float64(envInt("ZW_RATE_LIMIT_RPS", 200)), "per-client requests/second")
	fs.IntVar(&c.RateLimitBurst, "rate-limit-burst", envInt("ZW_RATE_LIMIT_BURST", 400), "per-client burst")
	cors := fs.String("cors-origins", env("ZW_CORS_ORIGINS", ""), "comma-separated allowed CORS origins")
	gen := fs.String("generate", env("ZW_GENERATE", "auto"), "synthetic RED/log generator: auto|true|false (auto = on for role=all)")
	fs.IntVar(&c.GenerateServices, "generate-services", envInt("ZW_GENERATE_SERVICES", 6), "number of synthetic services")
	sup := fs.String("supervisor", env("ZW_SUPERVISOR", "auto"), "RedBrain supervisor loop: auto|true|false")
	fs.DurationVar(&c.SupervisorEvery, "supervisor-interval", envDur("ZW_SUPERVISOR_INTERVAL", 10*time.Second), "supervisor loop interval")
	fs.StringVar(&c.BPFObject, "bpf-object", env("ZW_BPF_OBJECT", "ebpf/red_probe.bpf.o"), "path to compiled eBPF object (ebpf builds only)")
	fs.BoolVar(&c.LogDiscovery, "log-discovery", envBool("ZW_LOG_DISCOVERY", true), "supervisor discovers and tails host log files")
	fs.StringVar(&c.HostName, "hostname", env("ZW_HOSTNAME", ""), "Host dimension for this node's host metrics and RED (default: the OS hostname; in Kubernetes set it from spec.nodeName)")
	fs.BoolVar(&c.PrometheusAPI, "prometheus-api", envBool("ZW_PROMETHEUS_API", true), "serve the Prometheus-compatible query API at /prometheus/api/v1 (Grafana datasource URL http://host:8080/prometheus)")
	fs.StringVar(&c.OIDCIssuer, "oidc-issuer", env("ZW_OIDC_ISSUER", ""), "OpenID Connect issuer URL (exactly the tokens' iss); empty disables browser sign-in")
	fs.StringVar(&c.OIDCClientID, "oidc-client-id", env("ZW_OIDC_CLIENT_ID", ""), "OIDC client id")
	fs.StringVar(&c.OIDCRedirectURL, "oidc-redirect-url", env("ZW_OIDC_REDIRECT_URL", ""), "OIDC redirect URL registered at the provider (https://<host>/auth/callback)")
	fs.StringVar(&c.OIDCRoleClaim, "oidc-role-claim", env("ZW_OIDC_ROLE_CLAIM", "roles"), "ID token claim (dotted path) holding roles/groups")
	roleMap := fs.String("oidc-role-map", env("ZW_OIDC_ROLE_MAP", ""), "comma-separated claimValue=viewer|editor|admin")
	fs.StringVar(&c.OIDCDefaultRole, "oidc-default-role", env("ZW_OIDC_DEFAULT_ROLE", ""), "role when no claim value maps; empty denies sign-in")
	c.OIDCClientSecret = os.Getenv("ZW_OIDC_CLIENT_SECRET")
	c.SessionKey = os.Getenv("ZW_SESSION_KEY")
	fs.StringVar(&c.TierS3Endpoint, "tier-s3-endpoint", env("ZW_TIER_S3_ENDPOINT", ""), "S3/MinIO endpoint for the cold tier (empty = https://s3.<region>.amazonaws.com)")
	fs.StringVar(&c.TierBucket, "tier-bucket", env("ZW_TIER_BUCKET", ""), "cold-tier bucket; empty = no tiering (retention still applies locally)")
	fs.StringVar(&c.TierPrefix, "tier-prefix", env("ZW_TIER_PREFIX", "zerowatch"), "object key prefix; use one per storage node")
	fs.StringVar(&c.TierRegion, "tier-region", env("ZW_TIER_REGION", "us-east-1"), "bucket region")
	fs.BoolVar(&c.TierPathStyle, "tier-path-style", envBool("ZW_TIER_PATH_STYLE", true), "path-style addressing (MinIO); false = virtual-hosted")
	fs.DurationVar(&c.TierHot, "tier-hot", envDur("ZW_TIER_HOT", 24*time.Hour), "raw data stays in memory this long before moving to the bucket")
	fs.StringVar(&c.Retention, "retention", env("ZW_RETENTION", ""), "retention policy, e.g. raw=7d,rollup=90d,logs=7d (rawhot= also accepted)")
	if v := env("ZW_RETENTION_TENANT", ""); v != "" {
		c.RetentionTenant = strings.Split(v, ";")
	}
	fs.Func("retention-tenant", "per-tenant policy <tenant>:<policy> (repeatable, or ';'-joined)", func(s string) error {
		c.RetentionTenant = append(c.RetentionTenant, strings.Split(s, ";")...)
		return nil
	})
	fs.BoolVar(&c.HostMetrics, "host-metrics", envBool("ZW_HOST_METRICS", true), "supervisor collects node CPU, memory, disk and network metrics (namespace ZeroWatch/Host)")
	fs.StringVar(&c.HostRoot, "host-root", env("ZW_HOST_ROOT", ""), "prefix for host filesystem paths when running in a container (e.g. /host with -v /:/host:ro)")
	logRoots := fs.String("log-roots", env("ZW_LOG_ROOTS", "/var/log"), "comma-separated roots scanned for log files")
	fs.DurationVar(&c.RawRetention, "raw-retention", envDur("ZW_RAW_RETENTION", 6*time.Hour), "raw sample retention (memory engine)")
	fs.DurationVar(&c.RollupRetention, "rollup-retention", envDur("ZW_ROLLUP_RETENTION", 15*24*time.Hour), "1m rollup retention (memory engine)")
	fs.IntVar(&c.MaxSeries, "max-series", envInt("ZW_MAX_SERIES", 200000), "cardinality guard per process")
	fs.IntVar(&c.MaxLogEvents, "max-log-events", envInt("ZW_MAX_LOG_EVENTS", 200000), "log ring capacity (memory engine)")
	fs.IntVar(&c.GossipPort, "gossip-port", envInt("ZW_GOSSIP_PORT", 7946), "memberlist port (cluster mode)")
	seeds := fs.String("cluster-seeds", env("ZW_CLUSTER_SEEDS", ""), "comma-separated gossip seeds; empty = standalone")
	fs.StringVar(&c.ForecasterURL, "forecaster-url", env("ZW_FORECASTER_URL", ""), "model forecaster service URL (see forecaster/); empty = built-in methods only")
	reportTargets := fs.String("report-targets", env("ZW_REPORT_TARGETS", ""), "comma-separated report destinations: slack:<url>, webhook:<url>, log")
	fs.DurationVar(&c.ReportEvery, "report-every", envDur("ZW_REPORT_EVERY", 0), "send the summary report on this interval (e.g. 24h); 0 disables")
	fs.StringVar(&c.ReportRange, "report-range", env("ZW_REPORT_RANGE", "24h"), "range covered by scheduled reports: 24h|7d|30d")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", envDur("ZW_SHUTDOWN_TIMEOUT", 15*time.Second), "graceful shutdown timeout")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.KafkaBrokers = splitList(*brokers)
	c.ClusterSeeds = splitList(*seeds)
	c.CORSOrigins = splitList(*cors)
	c.LogRoots = splitList(*logRoots)
	c.ReportTargets = splitList(*reportTargets)
	c.RateLimitRPS = *rps
	var err error
	if c.OIDCRoleMap, err = ParseRoleMap(*roleMap); err != nil {
		return nil, err
	}
	if c.APIKeys, err = ParseAPIKeys(*keys); err != nil {
		return nil, err
	}
	if c.AWSCredentials, err = ParseAWSCredentials(*awsCreds); err != nil {
		return nil, err
	}
	c.Generate = resolveAuto(*gen, c.Role == RoleAll)
	c.Supervisor = resolveAuto(*sup, c.Role == RoleAll || c.Role == RoleSupervisor)
	if c.MetadataDSN == "" {
		c.MetadataDSN = "sqlite:" + strings.TrimRight(c.DataDir, "/") + "/metadata.db"
	}
	return c, c.Validate()
}

func resolveAuto(v string, def bool) bool {
	switch strings.ToLower(v) {
	case "auto", "":
		return def
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Validate checks semantic constraints.
func (c *Config) Validate() error {
	if c.OIDCIssuer != "" {
		if c.OIDCClientID == "" || c.OIDCRedirectURL == "" {
			return errors.New("config: --oidc-issuer needs --oidc-client-id and --oidc-redirect-url")
		}
		if c.OIDCDefaultRole != "" && !validRole(c.OIDCDefaultRole) {
			return errors.New("config: --oidc-default-role must be viewer, editor, admin or empty")
		}
	}
	if c.SessionKey != "" && len(c.SessionKey) < 32 {
		return errors.New("config: ZW_SESSION_KEY must be at least 32 bytes")
	}
	switch c.Role {
	case RoleAll, RoleIngest, RoleQuery, RoleStorage, RoleSupervisor:
	default:
		return fmt.Errorf("config: unknown role %q", c.Role)
	}
	switch c.StorageBackend {
	case "memory", "clickhouse":
	default:
		return fmt.Errorf("config: unknown storage backend %q", c.StorageBackend)
	}
	switch c.CommitLog {
	case "memory", "kafka":
	default:
		return fmt.Errorf("config: unknown commitlog %q", c.CommitLog)
	}
	if c.RateLimitRPS <= 0 || c.RateLimitBurst <= 0 {
		return errors.New("config: rate limits must be positive")
	}
	if !strings.HasPrefix(c.MetadataDSN, "sqlite:") && !strings.HasPrefix(c.MetadataDSN, "postgres://") && !strings.HasPrefix(c.MetadataDSN, "postgresql://") {
		return errors.New("config: metadata-dsn must start with sqlite: or postgres://")
	}
	switch c.ReportRange {
	case "24h", "7d", "30d":
	default:
		return fmt.Errorf("config: report-range must be 24h, 7d or 30d")
	}
	if c.ReportEvery < 0 || c.ReportEvery > 0 && c.ReportEvery < time.Minute {
		return errors.New("config: report-every must be 0 or at least 1m")
	}
	if c.ForecasterURL != "" && !strings.HasPrefix(c.ForecasterURL, "http://") && !strings.HasPrefix(c.ForecasterURL, "https://") {
		return errors.New("config: forecaster-url must be an http(s) URL")
	}
	if c.MaxSeries <= 0 || c.MaxLogEvents <= 0 {
		return errors.New("config: max-series and max-log-events must be positive")
	}
	return nil
}

// AuthEnabled reports whether API keys are enforced.
func (c *Config) AuthEnabled() bool {
	return len(c.APIKeys) > 0 || len(c.AWSCredentials) > 0 || c.OIDCIssuer != ""
}

// Runs reports whether a component should run for the configured role.
func (c *Config) Runs(role string) bool { return c.Role == RoleAll || c.Role == role }
