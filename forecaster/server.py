"""ZeroWatch model forecaster: an optional sidecar for pretrained time-series
foundation models. ZeroWatch calls it when started with --forecaster-url and
falls back to its built-in methods (Holt-Winters, STL, damped Holt) on any
error, so this service can be stopped at any time.

Protocol (internal/forecast/service.go callModel):

  POST /forecast
    {"values": [number|null, ...], "period": 60, "horizon": 60,
     "season": 1440, "interval": 0.8}
  200 {"model": "chronos-bolt-small", "forecast": [...], "lower": [...], "upper": [...]}

  GET /healthz -> {"status": "ok", "model": "..."}

Backends (env ZW_MODEL):
  chronos-bolt-tiny | chronos-bolt-mini | chronos-bolt-small (default) | chronos-bolt-base
  timesfm            google/timesfm-2.5-200m-pytorch (horizons up to 1024 steps)
  seasonal-naive     no ML; repeats the last season (for tests and as a floor)

Only the standard library plus numpy is needed for seasonal-naive; the
model backends need torch and the model package (see requirements.txt).
"""

from __future__ import annotations

import json
import logging
import math
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np

log = logging.getLogger("forecaster")

MAX_VALUES = 20000
MAX_HORIZON = 5000
MAX_BODY = 4 << 20


def fill_missing(values: list) -> np.ndarray:
    """Drop leading nulls, interpolate interior gaps, carry the last value."""
    y = np.array([np.nan if v is None else float(v) for v in values], dtype=np.float64)
    y[~np.isfinite(y)] = np.nan
    ok = np.flatnonzero(~np.isnan(y))
    if ok.size == 0:
        return np.array([], dtype=np.float64)
    y = y[ok[0]:]
    idx = np.arange(y.size)
    good = ~np.isnan(y)
    return np.interp(idx, idx[good], y[good])


def quantile_levels(interval: float) -> tuple[float, float]:
    lo = (1 - interval) / 2
    return lo, 1 - lo


class SeasonalNaive:
    name = "seasonal-naive"

    def predict(self, y: np.ndarray, horizon: int, season: int, interval: float):
        m = season if 2 <= season <= y.size // 2 else 1
        base = y[-m:]
        f = np.array([base[h % m] for h in range(horizon)])
        resid = y[m:] - y[:-m] if y.size > m else np.zeros(1)
        sigma = float(np.std(resid)) if resid.size > 1 else 0.0
        z = _normal_quantile((1 + interval) / 2)
        k = np.sqrt(np.floor(np.arange(horizon) / m) + 1)
        return f, f - z * sigma * k, f + z * sigma * k


class ChronosBolt:
    """amazon/chronos-bolt-* via the chronos-forecasting package (Apache-2.0)."""

    CONTEXT = 2048

    def __init__(self, size: str):
        import torch
        from chronos import BaseChronosPipeline

        self.torch = torch
        self.name = f"chronos-bolt-{size}"
        self.pipe = BaseChronosPipeline.from_pretrained(
            f"amazon/chronos-bolt-{size}", device_map="cpu", torch_dtype=torch.float32
        )

    def predict(self, y: np.ndarray, horizon: int, season: int, interval: float):
        lo, hi = quantile_levels(interval)
        # Bolt was trained on deciles 0.1..0.9; outside that it would extrapolate.
        lo, hi = max(lo, 0.1), min(hi, 0.9)
        ctx = self.torch.tensor(y[-self.CONTEXT:], dtype=self.torch.float32)
        q, _mean = self.pipe.predict_quantiles(ctx, prediction_length=horizon, quantile_levels=[lo, 0.5, hi])
        q = q[0].numpy()
        return q[:, 1], q[:, 0], q[:, 2]


class TimesFM:
    """google/timesfm-2.5-200m-pytorch via the timesfm package (Apache-2.0).

    Compiled for up to MAX_H future steps; longer horizons are refused (400),
    so ZeroWatch falls back to its built-in methods for them.
    """

    CONTEXT = 2048
    MAX_H = 1024

    def __init__(self):
        import timesfm

        self.name = "timesfm-2.5-200m"
        self.model = timesfm.TimesFM_2p5_200M_torch.from_pretrained("google/timesfm-2.5-200m-pytorch")
        self.model.compile(timesfm.ForecastConfig(
            max_context=self.CONTEXT, max_horizon=self.MAX_H, normalize_inputs=True,
            use_continuous_quantile_head=True, force_flip_invariance=True,
            infer_is_positive=True, fix_quantile_crossing=True,
        ))

    def predict(self, y: np.ndarray, horizon: int, season: int, interval: float):
        if horizon > self.MAX_H:
            raise BadRequest(f"{self.name} forecasts at most {self.MAX_H} steps")
        point, quant = self.model.forecast(horizon=horizon, inputs=[y[-self.CONTEXT:]])
        # quant[..., 0] is the mean, then the 10th..90th percentiles.
        lo, hi = quantile_levels(interval)
        li = int(round(min(max(lo, 0.1), 0.9) * 10))
        hi_i = int(round(min(max(hi, 0.1), 0.9) * 10))
        return point[0], quant[0, :, li], quant[0, :, hi_i]


def _normal_quantile(p: float) -> float:
    # Acklam's approximation, the same as internal/forecast.NormalQuantile.
    a = [-3.969683028665376e01, 2.209460984245205e02, -2.759285104469687e02, 1.383577518672690e02, -3.066479806614716e01, 2.506628277459239e00]
    b = [-5.447609879822406e01, 1.615858368580409e02, -1.556989798598866e02, 6.680131188771972e01, -1.328068155288572e01]
    c = [-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e00, -2.549732539343734e00, 4.374664141464968e00, 2.938163982698783e00]
    d = [7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e00, 3.754408661907416e00]
    if p < 0.02425:
        q = math.sqrt(-2 * math.log(p))
        return (((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) * q + c[5]) / ((((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1)
    if p > 1 - 0.02425:
        return -_normal_quantile(1 - p)
    q = p - 0.5
    r = q * q
    return (((((a[0] * r + a[1]) * r + a[2]) * r + a[3]) * r + a[4]) * r + a[5]) * q / (((((b[0] * r + b[1]) * r + b[2]) * r + b[3]) * r + b[4]) * r + 1)


def load_backend(name: str):
    if name == "seasonal-naive":
        return SeasonalNaive()
    if name.startswith("chronos-bolt-"):
        return ChronosBolt(name.removeprefix("chronos-bolt-"))
    if name == "timesfm":
        return TimesFM()
    raise SystemExit(f"unknown ZW_MODEL {name!r}")


class BadRequest(Exception):
    pass


def handle_forecast(backend, lock: threading.Lock, body: dict) -> dict:
    values = body.get("values")
    if not isinstance(values, list) or not values:
        raise BadRequest("values must be a non-empty list")
    if len(values) > MAX_VALUES:
        raise BadRequest(f"at most {MAX_VALUES} values")
    horizon = body.get("horizon")
    if not isinstance(horizon, int) or not 1 <= horizon <= MAX_HORIZON:
        raise BadRequest(f"horizon must be an integer in [1,{MAX_HORIZON}]")
    season = body.get("season") or 0
    interval = body.get("interval") or 0.8
    if not isinstance(season, int) or not isinstance(interval, (int, float)) or not 0 < interval < 1:
        raise BadRequest("season must be an integer and interval in (0,1)")
    y = fill_missing(values)
    if y.size < 3:
        raise BadRequest("need at least 3 non-null values")
    with lock:  # torch models are not re-entrant on CPU; one request at a time
        f, lo, hi = backend.predict(y, horizon, season, float(interval))
    f, lo, hi = (np.asarray(a, dtype=np.float64) for a in (f, lo, hi))
    lo, hi = np.minimum(lo, f), np.maximum(hi, f)
    if bool((y >= 0).all()):  # counts and latencies never go negative
        f, lo, hi = (np.maximum(a, 0) for a in (f, lo, hi))
    if not all(np.isfinite(a).all() for a in (f, lo, hi)):
        raise RuntimeError("model produced non-finite values")
    return {"model": backend.name, "forecast": f.tolist(), "lower": lo.tolist(), "upper": hi.tolist()}


def make_handler(backend, lock):
    class Handler(BaseHTTPRequestHandler):
        server_version = "zerowatch-forecaster/1.0"

        def _send(self, code: int, obj: dict):
            b = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)

        def do_GET(self):
            if self.path == "/healthz":
                self._send(200, {"status": "ok", "model": backend.name})
            else:
                self._send(404, {"error": "not found"})

        def do_POST(self):
            if self.path != "/forecast":
                self._send(404, {"error": "not found"})
                return
            n = int(self.headers.get("Content-Length") or 0)
            if n <= 0 or n > MAX_BODY:
                self._send(413 if n > MAX_BODY else 400, {"error": "body required (max 4 MiB)"})
                return
            t0 = time.perf_counter()
            try:
                out = handle_forecast(backend, lock, json.loads(self.rfile.read(n)))
            except (BadRequest, ValueError) as e:
                self._send(400, {"error": str(e)})
                return
            except Exception as e:  # noqa: BLE001 - report, ZeroWatch falls back
                log.exception("forecast failed")
                self._send(500, {"error": f"{type(e).__name__}: {e}"})
                return
            log.info("forecast h=%d %.0f ms", len(out["forecast"]), (time.perf_counter() - t0) * 1000)
            self._send(200, out)

        def log_message(self, fmt, *args):  # quiet default access log
            log.debug(fmt, *args)

    return Handler


def main():
    logging.basicConfig(level=os.environ.get("ZW_LOG_LEVEL", "INFO"), stream=sys.stderr,
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    name = os.environ.get("ZW_MODEL", "chronos-bolt-small")
    addr = os.environ.get("ZW_FORECASTER_ADDR", "0.0.0.0:8090")
    t0 = time.perf_counter()
    backend = load_backend(name)
    log.info("loaded %s in %.1f s", backend.name, time.perf_counter() - t0)
    host, port = addr.rsplit(":", 1)
    srv = ThreadingHTTPServer((host, int(port)), make_handler(backend, threading.Lock()))
    log.info("listening on %s", addr)
    srv.serve_forever()


if __name__ == "__main__":
    main()
