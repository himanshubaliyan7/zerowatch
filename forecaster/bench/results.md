# Forecast benchmark

## Session 4 (2026-10-16): short-history seasons and horizon-dependent calibration

Changes in `internal/forecast`: on history shorter than two seasons, auto also tries seasonal-naive with the configured season and Holt-Winters / STL / seasonal-naive with shorter seasons found by autocorrelation; calibration uses up to 8 holdouts, a conformal quantile and per-horizon-bucket scales (isotonic); Holt-Winters uses the additive h-step variance, the whole-sample one-step sigma, and a detrended seasonal start.

Run on a fresh binary (`ZW_URL`) scored on zw-bench's real series (`ZW_DATA_URL`), frozen in `results-s4-real.json` (`--real-cache`) so the old binary (zw-bench) was scored on exactly the same data: its run is `results-s4-baseline.json`. Sidecar: Chronos only (fc-timesfm was not running).

### Before / after, same data (geometric mean MASE; cover80 over all series)

| method | h=10 | h=12 | h=30 | h=48 | h=60 | h=288 | cover80 all | cover80 synthetic | cover80 real |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| zw:holtwinters before | 1.532 | 0.509 | 3.209 | 0.649 | 4.390 | 1.514 | 0.81 | 0.80 | 0.82 |
| zw:holtwinters after | 1.546 | 0.352 | 3.146 | 0.401 | 4.390 | 1.031 | 0.83 | 0.79 | 0.86 |
| zw:stl before | 1.532 | 0.444 | 3.209 | 0.493 | 4.390 | 0.906 | 0.73 | 0.62 | 0.82 |
| zw:stl after | 1.447 | 0.444 | 3.148 | 0.493 | 4.390 | 0.906 | 0.86 | 0.85 | 0.86 |
| zw:holt before | 1.532 | 0.317 | 3.209 | 0.466 | 4.390 | 1.761 | 0.86 | 0.91 | 0.82 |
| zw:holt after | 1.532 | 0.317 | 3.209 | 0.466 | 4.390 | 1.761 | 0.86 | 0.84 | 0.87 |
| zw:auto before | 1.532 | 0.337 | 3.209 | 0.303 | 4.390 | 0.798 | 0.83 | 0.85 | 0.82 |
| zw:auto after | 0.822 | 0.293 | 2.070 | 0.304 | 4.390 | 0.829 | 0.84 | 0.83 | 0.85 |

Real Requests series, zw:auto MASE before → after: auth 7.66 → 3.15, catalog 6.67 → 4.40, checkout 6.03 → 2.52, inventory 14.34 → 5.33, payments 6.35 → 3.44, search 6.13 → 2.78 (seasonal-naive 5.1–10.2, Chronos 1.8–3.1). h=60 is unchanged: its only origin has about 70 points, too few to validate a 60-point season.

Rolling origin, 5-minute buckets for the synthetic suite (season 288, 2016 points of history), 1-minute buckets and a 60-point season for real series. Lower MASE is better (1.0 = seasonal-naive in-sample error); cover80 should be near 0.80.

### Overall (geometric mean MASE per horizon)

| method | h=10 | h=12 | h=30 | h=48 | h=60 | h=288 | sMAPE % | cover80 | p50 ms |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| seasonal-naive | 1.668 | 0.815 | 2.735 | 0.775 | 2.920 | 1.230 | 18.6 | 0.60 | 0 |
| zw:holtwinters | 1.546 | 0.352 | 3.146 | 0.401 | 4.390 | 1.031 | 23.7 | 0.83 | 2 |
| zw:stl | 1.447 | 0.444 | 3.148 | 0.493 | 4.390 | 0.906 | 25.3 | 0.86 | 2 |
| zw:holt | 1.532 | 0.317 | 3.209 | 0.466 | 4.390 | 1.761 | 24.5 | 0.86 | 2 |
| zw:auto | 0.822 | 0.293 | 2.070 | 0.304 | 4.390 | 0.829 | 21.6 | 0.84 | 2 |
| model:chronos-bolt-small | 0.663 | 0.260 | 1.198 | 0.278 | 2.001 | 0.713 | 16.0 | 0.80 | 25 |

### Per series (geometric mean MASE over horizons and origins)

| series | seasonal-naive | zw:holtwinters | zw:stl | zw:holt | zw:auto | model:chronos-bolt-small |
|---|---:|---:|---:|---:|---:|---:|
| disk: linear fill | 1.000 | 0.040 | 0.051 | 0.094 | **0.040** | 0.080 |
| errors: sparse counts | 0.745 | 0.757 | 0.668 | 0.715 | 0.765 | **0.562** |
| latency p99: spiky | 0.791 | 0.651 | 0.614 | 0.685 | 0.597 | **0.431** |
| memory: random walk | 0.676 | 0.767 | 1.167 | **0.228** | 0.282 | 0.240 |
| real auth: Latency p99 | 0.484 | 0.523 | 0.523 | 0.523 | 0.555 | **0.391** |
| real auth: Requests Sum | 10.210 | 7.448 | 7.552 | 7.664 | 3.154 | **2.743** |
| real catalog: Latency p99 | 1.026 | 1.575 | 1.575 | 1.575 | 1.558 | **0.940** |
| real catalog: Requests Sum | 7.073 | 6.665 | 6.665 | 6.665 | 4.400 | **2.769** |
| real checkout: Latency p99 | 0.860 | 0.794 | 0.794 | 0.794 | 0.834 | **0.469** |
| real checkout: Requests Sum | 6.366 | 5.194 | 5.809 | 6.030 | 2.524 | **2.112** |
| real inventory: Latency p99 | 0.518 | 1.664 | 1.173 | 1.189 | 0.640 | **0.231** |
| real inventory: Requests Sum | 10.124 | 13.560 | 13.691 | 14.335 | 5.330 | **3.113** |
| real payments: Latency p99 | 0.745 | 0.723 | 0.723 | 0.723 | 0.991 | **0.554** |
| real payments: Requests Sum | 7.322 | 6.252 | 5.721 | 6.351 | 3.440 | **2.353** |
| real search: Latency p99 | 0.805 | 1.135 | 1.135 | 1.135 | 1.135 | **0.598** |
| real search: Requests Sum | 5.097 | 5.354 | 5.402 | 6.131 | 2.775 | **1.823** |
| traffic: daily | 1.027 | 0.889 | 0.987 | 2.083 | 0.891 | **0.718** |
| traffic: growth | 0.910 | 1.378 | 0.620 | 1.634 | 0.646 | **0.544** |
| traffic: level shift | 0.663 | 0.579 | 0.968 | 0.919 | 0.593 | **0.463** |
| traffic: weekly dip | 2.062 | 0.546 | 0.930 | 0.842 | 0.550 | **0.439** |

zw:auto picked: holt ×85, holtwinters ×32, snaive ×32, stl ×19

Methods actually used (fallbacks): zw:holtwinters → holt, zw:stl → holt

## Session 3 (earlier run, Chronos and TimesFM, real series about 66 min old)

Rolling origin, 5-minute buckets for the synthetic suite (season 288, 2016 points of history), 1-minute buckets and a 60-point season for real series. Lower MASE is better (1.0 = seasonal-naive in-sample error); cover80 should be near 0.80.

### Overall (geometric mean MASE per horizon)

| method | h=10 | h=12 | h=30 | h=48 | h=60 | h=288 | sMAPE % | cover80 | p50 ms |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| seasonal-naive | 1.574 | 0.815 | 2.538 | 0.775 | 2.662 | 1.230 | 18.6 | 0.60 | 0 |
| zw:holtwinters | 1.424 | 0.509 | 3.708 | 0.649 | 3.610 | 1.514 | 24.9 | 0.78 | 2 |
| zw:stl | 1.424 | 0.444 | 3.708 | 0.493 | 3.610 | 0.906 | 27.0 | 0.70 | 1 |
| zw:holt | 1.424 | 0.317 | 3.708 | 0.466 | 3.610 | 1.761 | 25.2 | 0.83 | 1 |
| zw:auto | 1.424 | 0.337 | 3.708 | 0.303 | 3.610 | 0.798 | 25.0 | 0.80 | 1 |
| model:chronos-bolt-small | 0.706 | 0.260 | 1.954 | 0.278 | 2.678 | 0.713 | 18.2 | 0.82 | 21 |
| model:timesfm-2.5-200m | 0.572 | 0.243 | 1.255 | 0.253 | 1.380 | 0.660 | 16.1 | 0.83 | 1463 |

### Per series (geometric mean MASE over horizons and origins)

| series | seasonal-naive | zw:holtwinters | zw:stl | zw:holt | zw:auto | model:chronos-bolt-small | model:timesfm-2.5-200m |
|---|---:|---:|---:|---:|---:|---:|---:|
| disk: linear fill | 1.000 | 0.931 | 0.051 | 0.094 | 0.062 | 0.080 | **0.042** |
| errors: sparse counts | 0.745 | 0.744 | 0.668 | 0.715 | 0.767 | 0.562 | **0.558** |
| latency p99: spiky | 0.791 | 0.639 | 0.614 | 0.685 | 0.589 | 0.431 | **0.426** |
| memory: random walk | 0.676 | 0.854 | 1.167 | **0.228** | 0.235 | 0.240 | 0.233 |
| real auth: Latency p99 | 0.486 | 0.640 | 0.640 | 0.640 | 0.640 | **0.419** | 0.429 |
| real auth: Requests Sum | 8.385 | 8.577 | 8.577 | 8.577 | 8.577 | 3.206 | **1.402** |
| real catalog: Latency p99 | **0.320** | 0.621 | 0.621 | 0.621 | 0.621 | 0.333 | 0.344 |
| real catalog: Requests Sum | 7.441 | 5.781 | 5.781 | 5.781 | 5.781 | 3.850 | **1.873** |
| real checkout: Latency p99 | 0.416 | 0.711 | 0.711 | 0.711 | 0.711 | 0.446 | **0.405** |
| real checkout: Requests Sum | 7.250 | 4.583 | 4.583 | 4.583 | 4.583 | 1.939 | **1.229** |
| real inventory: Latency p99 | 1.343 | 2.334 | 2.334 | 2.334 | 2.334 | **0.957** | 1.083 |
| real inventory: Requests Sum | 6.963 | 9.734 | 9.734 | 9.734 | 9.734 | 5.146 | **1.087** |
| real payments: Latency p99 | 0.597 | 0.828 | 0.828 | 0.828 | 0.828 | 0.580 | **0.563** |
| real payments: Requests Sum | 8.136 | 7.875 | 7.875 | 7.875 | 7.875 | 4.136 | **2.502** |
| real search: Latency p99 | 0.697 | 0.960 | 0.960 | 0.960 | 0.960 | 0.662 | **0.660** |
| real search: Requests Sum | 7.170 | 5.161 | 5.161 | 5.161 | 5.161 | 2.432 | **1.498** |
| traffic: daily | 1.027 | 0.888 | 0.987 | 2.083 | 0.891 | 0.718 | **0.705** |
| traffic: growth | 0.910 | 1.470 | 0.620 | 1.634 | 0.646 | **0.544** | 0.583 |
| traffic: level shift | 0.663 | 0.580 | 0.968 | 0.919 | 0.594 | **0.463** | 0.482 |
| traffic: weekly dip | 2.062 | 0.550 | 0.930 | 0.842 | 0.554 | 0.439 | **0.430** |

zw:auto picked: holt ×106, holtwinters ×29, stl ×21

Methods actually used (fallbacks): zw:holtwinters → holt, zw:stl → holt
