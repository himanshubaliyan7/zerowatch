# ZeroWatch backing services (docker compose)

`docker-compose.yml` runs the two external services ZeroWatch can use instead
of its embedded defaults:

| Service | Image | Ports (host) | Used by |
|---|---|---|---|
| ClickHouse | `clickhouse/clickhouse-server:24.8.14.39` (24.8 LTS) | 8123 (HTTP) | `--storage=clickhouse` (`internal/storage/clickhouse.go`) |
| Redpanda (Kafka API) | `docker.redpanda.com/redpandadata/redpanda:v24.2.18` | 19092 (Kafka) | `--commitlog=kafka` (`internal/commitlog/kafka.go`) |

ClickHouse user and password are both `zw`. Redpanda runs in dev-container
mode, with topic auto-creation on. Inside the compose network the services are
`clickhouse:8123` and `redpanda:9092`; from the host they are
`localhost:8123` and `localhost:19092`.

## Running the integration tests

The tests are behind the `integration` build tag. Each one is skipped unless
its environment variable is set:

| Variable | Example | Package |
|---|---|---|
| `ZW_TEST_CLICKHOUSE_URL` | `http://zw:zw@clickhouse:8123` (credentials as userinfo) | `./internal/storage` |
| `ZW_TEST_KAFKA_BROKERS` | `redpanda:9092` (comma separated) | `./internal/commitlog` |

Go isn't installed on the Windows host, so run the tests in a `golang:1.23`
container attached to the compose network. From `D:\home\zerowatch`:

```sh
docker compose -f deploy/compose/docker-compose.yml up -d --wait

docker run --rm --network zw-integration_default \
  -e ZW_TEST_CLICKHOUSE_URL=http://zw:zw@clickhouse:8123 \
  -e ZW_TEST_KAFKA_BROKERS=redpanda:9092 \
  -v "D:\home\zerowatch:/src" -v zw-gomod:/go/pkg/mod -v zw-gocache:/root/.cache/go-build \
  -w /src golang:1.23 \
  go test -tags integration -count=1 -v ./internal/storage ./internal/commitlog

docker compose -f deploy/compose/docker-compose.yml down -v
```

From Git Bash, put `MSYS_NO_PATHCONV=1` in front of `docker run`. With Go
installed locally, use the host ports instead:
`ZW_TEST_CLICKHOUSE_URL=http://zw:zw@localhost:8123 ZW_TEST_KAFKA_BROKERS=localhost:19092 go test -tags integration ./internal/storage ./internal/commitlog`.

The ClickHouse parity test takes about 2 minutes, because it runs about 19,000
queries one at a time. Each test creates its own database (`zw_it_*`) or topic
and consumer group (`zw-it-*`), and drops the databases afterwards.

### What the tests check

**ClickHouse** (`internal/storage/clickhouse_integration_test.go`). Each query
runs against the same data in `storage.NewMemory` and in ClickHouse, and the
two results must match.

- `EnsureSchema` can run again and again, and it upgrades tables made by the
  older schema in place.
- `FetchStat` is checked for every combination of:
  - Statistics: Average, Sum, Minimum, Maximum, SampleCount, p0, p50, p90, p99,
    p99.9 and p100.
  - Periods: 1 s, 10 s, 60 s and 300 s, both aligned and unaligned, including
    the memory engine's whole-minute selection on its rollup tier.
  - Dimension sets: exact matches, `*` wildcards, key-must-be-present,
    case-sensitive values, and hostile key and value strings.
  - StatisticSets, tenants, and metric names with injection characters.
- `ListSeries`, with namespace, metric, subset and limit filters.
- `SearchLogs`:
  - Terms, `key=value`, `-term` and quoted terms.
  - Case-insensitive non-ASCII text.
  - The event level and an attribute named `level`.
  - Level, group and stream filters, the time window, limit and truncation,
    and tenant 0, which means all tenants.
- `ListLogGroups` and `Stats`.

Results must be exact for counts, sums, minimums and maximums. Averages and
percentiles may differ by up to 1e-9 relative, because the summation order
differs; a run logs how many results weren't bit-for-bit equal.

**Kafka** (`internal/commitlog/kafka_integration_test.go`):

- A round trip of mixed metric and log batches, with every field and the order
  preserved.
- Offset commits, and a restarted consumer group resuming from where it
  stopped.
- A shutdown during the handler: an accepted batch is still committed, and an
  interrupted one is delivered again.
- Delivery again after a crash before the commit.
- A handler error is retried once; a batch that fails twice is dropped and
  committed.
- `ErrBackpressure` when the producer buffer is full.
- `Run` returns, and `Publish` returns `ErrClosed`, after `Close`.

## Known differences from the memory engine

These are by design, or not yet fixed. Each is noted in the code.

- **Lookback windows:**
  - `ListSeries` only sees series written in the last 24 h. The memory engine
    keeps a series until its retention runs out.
  - `ListLogGroups` summarises the last 24 h. The memory engine summarises its
    whole ring.
- **Log order:** `SearchLogs` returns the newest events by timestamp first.
  The memory engine returns them in reverse order of arrival. The two agree
  when events arrive in time order.
- **`Scanned`** is the number of events returned, not the number examined.
- **Index:** message terms are substring matches
  (`positionCaseInsensitiveUTF8`), the same as in the memory engine, so the
  `tokenbf_v1` index on `message` doesn't prune them.
- **Missing values:** a `MetricDatum` with no value, or a StatisticSet with
  `SampleCount` 0, writes no row. The memory engine still registers the
  series.
- **`Stats`:** `Series` is approximate (`uniq`) above about 65 k series, and
  `RollupBucket` is not reported.
- **Kafka:**
  - Publishes aren't all-or-none. When `ErrBackpressure` is returned, records
    already queued are still delivered.
  - Depth and Capacity are not reported in its stats.

## Cold tier: MinIO (`--profile tier`)

The `minio` service is an S3-compatible bucket for the cold tier
(`internal/tier`). It sits behind the `tier` profile, so `docker compose up`
still starts only ClickHouse and Redpanda.

| Service | Image | Ports (host) | Used by |
|---|---|---|---|
| MinIO | `cgr.dev/chainguard/minio:latest` | 9000 (S3), 9001 (console) | `--tier-s3-endpoint`, `--tier-bucket` |

`minio/minio` can no longer be pulled from Docker Hub or quay.io; the
Chainguard image is a build of the same server. The root user is `zwtier` and
the password `zwtiersecret`. Data lives in the container and is lost when it is
removed.

```sh
docker compose --profile tier up -d minio
ZW_TIER_ACCESS_KEY_ID=zwtier ZW_TIER_SECRET_ACCESS_KEY=zwtiersecret \
  zerowatch --tier-s3-endpoint=http://localhost:9000 --tier-bucket=zerowatch \
            --tier-prefix=node1 --tier-hot=24h --retention=raw=7d,rollup=90d,logs=7d
```

ZeroWatch creates the bucket when it is missing. Give each storage node its
own `--tier-prefix`: a node is the only writer of the manifests under its
prefix.

**Integration test** (3 days of data with a 1 h hot window; checks that
Average, Sum, Minimum, Maximum, SampleCount and percentiles over cold ranges
equal an all-hot engine, and that retention deletes hourly prefixes), from the
repository root:

```sh
docker network create w4-tier-net
docker run -d --rm --name w4-tier-minio --network w4-tier-net \
  -e MINIO_ROOT_USER=zwtieradmin -e MINIO_ROOT_PASSWORD=zwtiersecret123 \
  cgr.dev/chainguard/minio:latest server /tmp/data
docker run --rm --network w4-tier-net -v "$PWD:/src" -w /src \
  -e ZW_TIER_TEST_ENDPOINT=http://w4-tier-minio:9000 \
  -e ZW_TIER_ACCESS_KEY_ID=zwtieradmin -e ZW_TIER_SECRET_ACCESS_KEY=zwtiersecret123 \
  golang:1.23 go test -tags integration -run TestMinIO -v ./internal/tier/
docker stop w4-tier-minio && docker network rm w4-tier-net
```

The ClickHouse engine does not use the tier; it has its own TTLs.
