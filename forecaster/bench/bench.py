"""Forecast benchmark: ZeroWatch's built-in methods vs model sidecars.

Rolling-origin evaluation on a synthetic suite shaped like ZeroWatch data
(5-minute buckets, daily season) plus, optionally, real RED series read from
a running ZeroWatch.

  ZW_URL=http://localhost:8080 \\
  FORECASTERS=http://localhost:8090,http://localhost:8091 \\
  python bench/bench.py [--horizons 12,48,288] [--origins 3] [--real [--real-cache f.json]] [--out bench/results]

ZW_DATA_URL (default ZW_URL) is where --real reads its series, so a freshly
built binary (ZW_URL) can be scored on another server's history.

Built-ins are called through POST /api/v1/forecast with raw values (methods
holtwinters, stl, holt, and auto); each FORECASTERS entry is called with the
sidecar protocol directly; seasonal-naive is computed here as the floor.

Scores per (series, origin, horizon):
  MASE      mean absolute error / in-sample MAE of the seasonal-naive forecast
  sMAPE     symmetric mean absolute percentage error (%)
  cover80   share of actual values inside the method's 80 % interval
  ms        wall time of the call
Aggregates: geometric mean of MASE (robust to scale), means of the rest.
Writes <out>.md and <out>.json. Standard library + numpy only.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
import time
import urllib.request
from pathlib import Path

import numpy as np

PERIOD = 300  # seconds (switched to 60 for real series)
SYNTH_PERIOD = PERIOD
SEASON = 288  # one day of 5-minute buckets
HISTORY = 7 * SEASON
API_KEY = os.environ.get("ZW_API_KEY", "")


# --- synthetic suite --------------------------------------------------------


def daily(n, rng, base, amp, noise, phase=0.0):
    t = np.arange(n)
    day = np.sin(2 * np.pi * (t / SEASON) + phase)
    peak = np.exp(-((t % SEASON - SEASON * 0.6) ** 2) / (2 * (SEASON * 0.06) ** 2))  # evening peak
    return base + amp * day + 0.5 * amp * peak + rng.normal(0, noise, n)


def suite(seed=7):
    rng = np.random.default_rng(seed)
    n = HISTORY + SEASON  # room for the largest horizon
    t = np.arange(n)
    out = {
        "traffic: daily": np.maximum(daily(n, rng, 1000, 400, 40), 0),
        "traffic: growth": np.maximum(daily(n, rng, 800, 300, 30) * (1 + t / n * 0.8), 0),
        "traffic: weekly dip": np.maximum(daily(n, rng, 900, 350, 35) * np.where((t // SEASON) % 7 >= 5, 0.6, 1.0), 0),
    }
    step = daily(n, rng, 500, 150, 20)
    step[int(n * 0.8):] += 300
    out["traffic: level shift"] = np.maximum(step, 0)
    lat = 40 + 15 * np.sin(2 * np.pi * t / SEASON) + rng.gamma(2, 4, n)
    spikes = rng.random(n) < 0.01
    lat[spikes] *= rng.uniform(3, 8, spikes.sum())
    out["latency p99: spiky"] = lat
    out["errors: sparse counts"] = rng.poisson(np.maximum(0.5 + 0.4 * np.sin(2 * np.pi * t / SEASON), 0)).astype(float)
    out["disk: linear fill"] = 30 + 50 * t / n + rng.normal(0, 0.3, n)
    out["memory: random walk"] = 2000 + np.cumsum(rng.normal(0, 5, n))
    return out


# --- real series from ZeroWatch ---------------------------------------------


def http_json(url, body=None, timeout=120):
    headers = {"Content-Type": "application/json", "User-Agent": "zerowatch-bench"}
    if API_KEY:
        headers["X-API-Key"] = API_KEY
    req = urllib.request.Request(url, json.dumps(body).encode() if body is not None else None, headers)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)


def real_series(zw, period=60, hours=6):
    """Per-service Requests (Sum) and Latency (p99) at 1-minute buckets."""
    series = http_json(f"{zw}/api/v1/series?namespace=ZeroWatch/RED&metricName=Requests&limit=5000")["series"] or []
    services = sorted({s["dimensions"].get("Service") for s in series} - {None})
    end = int(time.time() * 1000) // (period * 1000) * period * 1000
    start = end - hours * 3600 * 1000
    out = {}
    for svc in services[:6]:
        for metric, stat in (("Requests", "Sum"), ("Latency", "p99")):
            q = {"queries": [{"id": "m", "metricStat": {"namespace": "ZeroWatch/RED", "metricName": metric, "stat": stat,
                                                         "dimensions": {"Service": svc, "Route": "*"}}}],
                 "start": start, "end": end, "period": period}
            r = http_json(f"{zw}/api/v1/query", q)["results"][0]
            v = np.full((end - start) // (period * 1000), np.nan)
            for ts, x in zip(r["timestamps"], r["values"]):
                v[(ts - start) // (period * 1000)] = x
            ok = np.flatnonzero(~np.isnan(v))
            if ok.size > 60:
                out[f"real {svc}: {metric} {stat}"] = v[ok[0]:]
    return out


# --- methods ----------------------------------------------------------------


def clean(y):
    return [None if not math.isfinite(x) else float(x) for x in y]


def builtin(zw, method):
    def run(ctx, h, season):
        r = http_json(f"{zw}/api/v1/forecast", {"values": clean(ctx), "period": PERIOD, "horizon": h, "season": season,
                                                "method": method, "interval": 0.8})
        p = r["points"]
        return [x["forecast"] for x in p], [x["lower"] for x in p], [x["upper"] for x in p], r["method"]
    return run


def sidecar(url):
    def run(ctx, h, season):
        r = http_json(f"{url}/forecast", {"values": clean(ctx), "period": PERIOD, "horizon": h, "season": season, "interval": 0.8})
        return r["forecast"], r["lower"], r["upper"], r["model"]
    return run


def seasonal_naive(ctx, h, season):
    m = season if len(ctx) >= 2 * season else 1
    base = ctx[-m:]
    f = np.array([base[i % m] for i in range(h)])
    d = ctx[m:] - ctx[:-m]
    s = np.nanstd(d) * 1.2816
    return f, f - s, f + s, "seasonal-naive"


# --- scoring ----------------------------------------------------------------


def scores(ctx, actual, f, lo, hi, season):
    f, lo, hi, actual = (np.asarray(a, float) for a in (f, lo, hi, actual))
    m = season if len(ctx) > season else 1
    scale = np.nanmean(np.abs(ctx[m:] - ctx[:-m]))
    mae = np.mean(np.abs(f - actual))
    denom = np.abs(f) + np.abs(actual)
    smape = np.mean(np.where(denom > 0, 2 * np.abs(f - actual) / np.where(denom > 0, denom, 1), 0)) * 100
    return {"mase": float(mae / scale) if scale > 0 else float("nan"), "smape": float(smape),
            "cover80": float(np.mean((actual >= lo) & (actual <= hi)))}


def evaluate(datasets, methods, horizons, origins, season_for):
    rows = []
    for name, y in datasets.items():
        season = season_for(name, y)
        for h in horizons:
            for k in range(origins):
                cut = len(y) - h - k * h
                if cut < 3:
                    continue
                ctx, actual = y[:cut], y[cut:cut + h]
                ctx = ctx[-min(len(ctx), HISTORY):]
                if np.isnan(actual).any():
                    continue
                for mname, fn in methods.items():
                    t0 = time.perf_counter()
                    try:
                        f, lo, hi, used = fn(ctx, h, season)
                    except Exception as e:  # noqa: BLE001
                        print(f"  {mname} on {name} h={h}: {e}", file=sys.stderr)
                        continue
                    ms = (time.perf_counter() - t0) * 1000
                    rows.append({"series": name, "h": h, "origin": k, "method": mname, "used": used, "ms": ms,
                                 **scores(ctx, actual, f, lo, hi, season)})
        print(f"done {name}", file=sys.stderr)
    return rows


def aggregate(rows, key):
    groups = {}
    for r in rows:
        groups.setdefault(key(r), []).append(r)
    out = {}
    for g, rs in groups.items():
        mase = [r["mase"] for r in rs if math.isfinite(r["mase"]) and r["mase"] > 0]
        out[g] = {"n": len(rs), "mase_gmean": float(np.exp(np.mean(np.log(mase)))) if mase else float("nan"),
                  "smape": float(np.mean([r["smape"] for r in rs])), "cover80": float(np.mean([r["cover80"] for r in rs])),
                  "ms_p50": float(np.median([r["ms"] for r in rs]))}
    return out


def markdown(rows, horizons, methods):
    lines = ["# Forecast benchmark", "",
             f"Rolling origin, {SYNTH_PERIOD // 60}-minute buckets for the synthetic suite (season {SEASON}, "
             f"{HISTORY} points of history), 1-minute buckets and a 60-point season for real series. Lower MASE is better (1.0 = seasonal-naive in-sample error); "
             "cover80 should be near 0.80.", ""]
    by = aggregate(rows, lambda r: (r["method"], r["h"]))
    lines += ["## Overall (geometric mean MASE per horizon)", "",
              "| method | " + " | ".join(f"h={h}" for h in horizons) + " | sMAPE % | cover80 | p50 ms |",
              "|---|" + "---:|" * (len(horizons) + 3)]
    allm = aggregate(rows, lambda r: r["method"])
    for m in methods:
        if m not in allm:
            continue
        cells = [f"{by[(m, h)]['mase_gmean']:.3f}" if (m, h) in by else "–" for h in horizons]
        a = allm[m]
        lines.append(f"| {m} | " + " | ".join(cells) + f" | {a['smape']:.1f} | {a['cover80']:.2f} | {a['ms_p50']:.0f} |")
    lines += ["", "## Per series (geometric mean MASE over horizons and origins)", ""]
    per = aggregate(rows, lambda r: (r["series"], r["method"]))
    series = sorted({r["series"] for r in rows})
    present = [m for m in methods if m in allm]
    lines += ["| series | " + " | ".join(present) + " |", "|---|" + "---:|" * len(present)]
    for s in series:
        vals = [per.get((s, m), {}).get("mase_gmean", float("nan")) for m in present]
        finite = [v for v in vals if math.isfinite(v)]
        best = min(finite) if finite else None
        cells = [("**%.3f**" if v == best else "%.3f") % v if math.isfinite(v) else "–" for v in vals]
        lines.append(f"| {s} | " + " | ".join(cells) + " |")
    base = lambda m: m.split(":", 1)[-1]
    used = sorted({(r["method"], r["used"]) for r in rows if base(r["method"]) not in (r["used"], "auto", "seasonal-naive")})
    auto = {}
    for r in rows:
        if r["method"] == "zw:auto":
            auto[r["used"]] = auto.get(r["used"], 0) + 1
    if auto:
        lines += ["", "zw:auto picked: " + ", ".join(f"{k} ×{v}" for k, v in sorted(auto.items()))]
    if used:
        lines += ["", "Methods actually used (fallbacks): " + ", ".join(f"{m} → {u}" for m, u in used)]
    return "\n".join(lines) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--horizons", default="12,48,288")
    ap.add_argument("--origins", type=int, default=3)
    ap.add_argument("--real", action="store_true", help="also benchmark real RED series from ZW_DATA_URL (default ZW_URL)")
    ap.add_argument("--real-cache", help="JSON file of real series: read if it exists, else written, so runs compare like for like")
    ap.add_argument("--out", default=str(Path(__file__).with_name("results")))
    a = ap.parse_args()
    zw = os.environ.get("ZW_URL", "").rstrip("/")
    if not zw:
        sys.exit("set ZW_URL to a running ZeroWatch (the built-in methods run there)")
    horizons = [int(x) for x in a.horizons.split(",")]
    methods = {"seasonal-naive": seasonal_naive}
    for m in ("holtwinters", "stl", "holt", "auto"):
        methods[f"zw:{m}"] = builtin(zw, m)
    for url in filter(None, os.environ.get("FORECASTERS", "").split(",")):
        url = url.rstrip("/")
        name = http_json(f"{url}/healthz")["model"]
        methods[f"model:{name}"] = sidecar(url)
    print("methods:", ", ".join(methods), file=sys.stderr)
    data = suite()
    rows = evaluate(data, methods, horizons, a.origins, lambda n, y: SEASON)
    if a.real:
        cache = Path(a.real_cache) if a.real_cache else None
        if cache and cache.exists():
            real = {k: np.array([np.nan if x is None else x for x in v], float)
                    for k, v in json.loads(cache.read_text(encoding="utf-8")).items()}
        else:
            real = real_series(os.environ.get("ZW_DATA_URL", zw).rstrip("/"))
            if cache:
                cache.write_text(json.dumps({k: clean(v) for k, v in real.items()}), encoding="utf-8")
        print(f"real series: {len(real)}", file=sys.stderr)
        # Real series are short (a fresh node): 1-minute buckets, 60-point season, horizons ≤ 60.
        global PERIOD
        PERIOD = 60
        rh = [h for h in (10, 30, 60)]
        rows += evaluate(real, methods, rh, a.origins, lambda n, y: 60)
        horizons = sorted(set(horizons) | set(rh))
    md = markdown(rows, horizons, list(methods))
    Path(a.out + ".md").write_text(md, encoding="utf-8")
    Path(a.out + ".json").write_text(json.dumps(rows, indent=1), encoding="utf-8")
    print(md)


if __name__ == "__main__":
    main()
