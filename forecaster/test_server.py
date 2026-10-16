"""Protocol tests with the seasonal-naive backend (no torch needed):
python -m unittest -v test_server
"""

import json
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer

import numpy as np

import server


class ForecasterTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = ThreadingHTTPServer(("127.0.0.1", 0), server.make_handler(server.SeasonalNaive(), threading.Lock()))
        cls.url = f"http://127.0.0.1:{cls.srv.server_address[1]}"
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def post(self, body):
        req = urllib.request.Request(self.url + "/forecast", json.dumps(body).encode(), {"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=5) as r:
            return json.load(r)

    def test_seasonal_forecast(self):
        y = [10 + (i % 4) for i in range(40)]
        y[5] = None
        out = self.post({"values": y, "period": 60, "horizon": 6, "season": 4, "interval": 0.8})
        self.assertEqual(out["model"], "seasonal-naive")
        self.assertEqual(out["forecast"], [10, 11, 12, 13, 10, 11])
        for f, lo, hi in zip(out["forecast"], out["lower"], out["upper"]):
            self.assertLessEqual(lo, f)
            self.assertLessEqual(f, hi)

    def test_bad_requests(self):
        for body in ({"values": [], "horizon": 1}, {"values": [1, 2, 3], "horizon": 0}, {"values": [None, 1], "horizon": 1},
                     {"values": [1, 2, 3], "horizon": 1, "interval": 2}):
            with self.assertRaises(urllib.error.HTTPError) as cm:
                self.post(body)
            self.assertEqual(cm.exception.code, 400, body)

    def test_healthz(self):
        with urllib.request.urlopen(self.url + "/healthz", timeout=5) as r:
            self.assertEqual(json.load(r)["model"], "seasonal-naive")

    def test_fill_missing(self):
        np.testing.assert_array_equal(server.fill_missing([None, 1, None, 3, None]), [1, 2, 3, 3])

    def test_nonnegative_clamp(self):
        out = self.post({"values": [5, 0, 5, 0, 5, 0, 5, 0], "horizon": 2, "season": 2, "interval": 0.99})
        self.assertGreaterEqual(min(out["lower"]), 0)


if __name__ == "__main__":
    unittest.main()
