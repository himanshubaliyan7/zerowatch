-- ZeroWatch ClickHouse schema, applied statement by statement by
-- ClickHouse.EnsureSchema. Every statement must be idempotent.
-- Raw metrics: DoubleDelta timestamps + Gorilla values, 14 day TTL.
-- value is the sample (a StatisticSet's average); stat_* carry the
-- (count, sum, min, max) summary so Sum/SampleCount/Minimum/Maximum of
-- pre-aggregated StatisticSets match the memory engine's rollup tier.
CREATE TABLE IF NOT EXISTS metrics_raw (
    tenant_id UInt32,
    series_id UInt64,
    timestamp DateTime64(3, 'UTC') CODEC(DoubleDelta, ZSTD(1)),
    value Float64 CODEC(Gorilla, ZSTD(1)),
    namespace LowCardinality(String),
    metric_name LowCardinality(String),
    dimensions Map(LowCardinality(String), String) CODEC(ZSTD(3)),
    unit LowCardinality(String) DEFAULT '',
    stat_count Float64 DEFAULT 1 CODEC(ZSTD(1)),
    stat_sum Float64 DEFAULT value CODEC(Gorilla, ZSTD(1)),
    stat_min Float64 DEFAULT value CODEC(Gorilla, ZSTD(1)),
    stat_max Float64 DEFAULT value CODEC(Gorilla, ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant_id, namespace, metric_name, series_id, timestamp)
TTL toDateTime(timestamp) + INTERVAL 14 DAY;

-- Upgrades for tables created before the columns above existed.
ALTER TABLE metrics_raw ADD COLUMN IF NOT EXISTS unit LowCardinality(String) DEFAULT '';
ALTER TABLE metrics_raw ADD COLUMN IF NOT EXISTS stat_count Float64 DEFAULT 1 CODEC(ZSTD(1));
ALTER TABLE metrics_raw ADD COLUMN IF NOT EXISTS stat_sum Float64 DEFAULT value CODEC(Gorilla, ZSTD(1));
ALTER TABLE metrics_raw ADD COLUMN IF NOT EXISTS stat_min Float64 DEFAULT value CODEC(Gorilla, ZSTD(1));
ALTER TABLE metrics_raw ADD COLUMN IF NOT EXISTS stat_max Float64 DEFAULT value CODEC(Gorilla, ZSTD(1));

-- Continuous 1-minute rollup (count/sum/min/max/percentile states).
CREATE MATERIALIZED VIEW IF NOT EXISTS metrics_1m_mv
ENGINE = AggregatingMergeTree()
PARTITION BY toYYYYMMDD(window_start)
ORDER BY (tenant_id, namespace, metric_name, series_id, window_start)
AS SELECT
    tenant_id,
    namespace,
    metric_name,
    series_id,
    toStartOfMinute(timestamp) AS window_start,
    sumState(stat_count) AS sample_count,
    sumState(stat_sum) AS sum_val,
    minState(stat_min) AS min_val,
    maxState(stat_max) AS max_val,
    quantilesExactWeightedState(0.5, 0.90, 0.99)(value, 1) AS percentiles
FROM metrics_raw
GROUP BY tenant_id, namespace, metric_name, series_id, window_start;

-- Logs with a token bloom-filter skip index, 30 day TTL. level is its own
-- column so an attribute named "level" never collides with the event level.
CREATE TABLE IF NOT EXISTS logs (
    tenant_id UInt32,
    log_group LowCardinality(String),
    log_stream String,
    timestamp DateTime64(3, 'UTC') CODEC(DoubleDelta, ZSTD(1)),
    message String CODEC(ZSTD(6)),
    level LowCardinality(String) DEFAULT '',
    attributes Map(String, String) CODEC(ZSTD(3)),
    INDEX idx_msg message TYPE tokenbf_v1(30720, 2, 0) GRANULARITY 1
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant_id, log_group, timestamp, log_stream)
TTL toDateTime(timestamp) + INTERVAL 30 DAY;

ALTER TABLE logs ADD COLUMN IF NOT EXISTS level LowCardinality(String) DEFAULT '' AFTER message;
