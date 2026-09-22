#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Inference / Serving Stage (Stage 3 of the ML Demo).

A tiny, dependency-light HTTP inference server that loads the model trained in
Stage 2 from GCS and serves predictions. Running on
regular CPU nodes (no TPU needed for this small MLP), it demonstrates the third
leg of the ML lifecycle — serving — inside the same Kubeflow / Kubernetes world.

Endpoints:
  GET  /healthz   -> readiness/liveness probe
  GET  /metrics   -> the metrics.json emitted by training
  POST /predict   -> body: {"instances": [[784 floats], ...]}
                     resp: {"predictions": [int, ...], "probabilities": [[...], ...]}
"""

import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import subprocess
import sys

# Ensure google-cloud-storage is installed
try:
    from google.cloud import storage
except ImportError:
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "google-cloud-storage"])
    from google.cloud import storage

BUCKET_NAME = os.environ.get("BUCKET_NAME")
if not BUCKET_NAME:
    raise ValueError("BUCKET_NAME environment variable must be set")

PORT = int(os.environ.get("PORT", "8080"))

CLASS_NAMES = [
    "T-shirt/top", "Trouser", "Pullover", "Dress", "Coat",
    "Sandal", "Shirt", "Sneaker", "Bag", "Ankle boot",
]

_params = None
_loaded_mtime = None


def load_model_if_ready():
    """Load (or hot-reload) params.npz from GCS if it exists and has changed."""
    global _params, _loaded_mtime
    client = storage.Client()
    bucket = client.bucket(BUCKET_NAME)
    blob = bucket.get_blob("model/params.npz")
    if not blob:
        return False
    
    updated = blob.updated
    if _params is not None and updated == _loaded_mtime:
        return True
    
    tmp_path = "/tmp/params.npz"
    blob.download_to_filename(tmp_path)
    d = np.load(tmp_path)
    _params = {k: d[k].astype(np.float32) for k in d.files}
    os.remove(tmp_path)
    
    _loaded_mtime = updated
    print(f"[serve] loaded model from gs://{BUCKET_NAME}/model/params.npz (updated={updated})", flush=True)
    return True


def forward(x):
    h = np.maximum(x @ _params["w1"] + _params["b1"], 0.0)
    logits = h @ _params["w2"] + _params["b2"]
    logits -= logits.max(axis=-1, keepdims=True)
    exp = np.exp(logits)
    return exp / exp.sum(axis=-1, keepdims=True)


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):  # silence default noisy logging
        pass

    def do_GET(self):
        if self.path == "/livez":
            self._json(200, {"status": "ok"})
        elif self.path == "/healthz":
            ready = load_model_if_ready()
            self._json(200 if ready else 503, {"model_loaded": ready})
        elif self.path == "/metrics":
            client = storage.Client()
            bucket = client.bucket(BUCKET_NAME)
            blob = bucket.get_blob("model/metrics.json")
            if blob:
                metrics_str = blob.download_as_text()
                self._json(200, json.loads(metrics_str))
            else:
                self._json(404, {"error": "metrics not available yet"})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/predict":
            self._json(404, {"error": "not found"})
            return
        if _params is None:
            self._json(503, {"error": "model not loaded yet"})
            return
        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length))
            x = np.asarray(body["instances"], dtype=np.float32)
            if x.ndim == 1:
                x = x[None, :]
            probs = forward(x)
            preds = probs.argmax(axis=-1).tolist()
            self._json(200, {
                "predictions": preds,
                "labels": [CLASS_NAMES[p] for p in preds],
                "probabilities": probs.round(4).tolist(),
            })
        except Exception as e:  # noqa: BLE001 - surface any request error as 400
            self._json(400, {"error": str(e)})


def main():
    print(f"[serve] starting inference server on :{PORT}", flush=True)
    print(f"[serve] waiting for model at gs://{BUCKET_NAME}/model/params.npz ...", flush=True)
    # Try an initial load, but start serving immediately — the readiness probe
    # (/healthz) gates traffic until params.npz appears in the bucket.
    load_model_if_ready()
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    server.serve_forever()


if __name__ == "__main__":
    main()
