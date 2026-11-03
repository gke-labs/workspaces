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

"""GPU Inference Service for MiniGPT.

A high-performance HTTP inference server deployed on NVIDIA Tesla T4 GPUs.
Loads weights trained on TPU or GPU directly from GCS, initializes the model on
CUDA, and serves autoregressive text generation.

Endpoints:
  GET  /healthz   -> Readiness/liveness probe
  GET  /metrics   -> Training and inference metrics
  POST /generate  -> Generates text from a prompt
"""

import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch

# Ensure local jobs modules (model.py) are discoverable
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
if SCRIPT_DIR not in sys.path:
    sys.path.insert(0, SCRIPT_DIR)
from model import MiniGPT

try:
    from transformers import GPT2Tokenizer
except ImportError:
    import subprocess
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "transformers<4.45.0"])
    from transformers import GPT2Tokenizer

try:
    from google.cloud import storage
except ImportError:
    import subprocess
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "google-cloud-storage"])
    from google.cloud import storage

BUCKET_NAME = os.environ.get("BUCKET_NAME") or os.environ.get("GCS_BUCKET")
if not BUCKET_NAME:
    raise ValueError("BUCKET_NAME or GCS_BUCKET environment variable must be set")

PORT = int(os.environ.get("PORT", "8080"))

_model = None
_tokenizer = None
_config = {}
_device = None


def load_model_from_gcs():
    """Downloads model weights and hyperparameters from GCS and loads onto CUDA GPU."""
    global _model, _tokenizer, _config, _device

    _device = torch.device("cuda:0" if torch.cuda.is_available() else "cpu")
    print(f"[Serving] Initializing inference device: {_device}", flush=True)
    if torch.cuda.is_available():
        print(f"[Serving] GPU Hardware: {torch.cuda.get_device_name(0)} ({torch.cuda.device_count()} GPU(s) available)", flush=True)

    client = storage.Client()
    bucket = client.bucket(BUCKET_NAME)

    # 1. Download config
    config_blob = bucket.get_blob("model/config.json")
    if config_blob:
        _config = json.loads(config_blob.download_as_text())
        print(f"[Serving] Loaded model config: {_config}", flush=True)
    else:
        _config = {
            "vocab_size": 50257,
            "block_size": 128,
            "n_layer": 4,
            "n_head": 4,
            "n_embd": 128,
            "trained_on": "unknown",
        }

    # 2. Download weights
    local_weights_path = "/tmp/minigpt.pt"
    print(f"[Serving] Downloading model weights from gs://{BUCKET_NAME}/model/minigpt.pt...", flush=True)
    model_blob = bucket.get_blob("model/minigpt.pt")
    if not model_blob:
        raise FileNotFoundError(f"Model blob 'model/minigpt.pt' not found in bucket '{BUCKET_NAME}'")
    model_blob.download_to_filename(local_weights_path)

    # 3. Instantiate model and load state_dict
    _model = MiniGPT(
        vocab_size=_config.get("vocab_size", 50257),
        block_size=_config.get("block_size", 128),
        n_layer=_config.get("n_layer", 4),
        n_head=_config.get("n_head", 4),
        n_embd=_config.get("n_embd", 128),
    )

    state_dict = torch.load(local_weights_path, map_location=_device, weights_only=True)
    _model.load_state_dict(state_dict)
    _model.to(_device)
    _model.eval()

    # 4. Tokenizer
    print("[Serving] Loading GPT-2 tokenizer...", flush=True)
    _tokenizer = GPT2Tokenizer.from_pretrained("gpt2")
    print(f"[Serving] MiniGPT model successfully loaded on {_device}! (Trained on: {_config.get('trained_on', 'unknown').upper()})", flush=True)


class InferenceHandler(BaseHTTPRequestHandler):
    """HTTP request handler for inference endpoints."""

    def _send_json(self, status: int, data: dict):
        body = json.dumps(data).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path in ("/healthz", "/livez"):
            ready = _model is not None and _tokenizer is not None
            if ready:
                self._send_json(200, {
                    "status": "ready",
                    "device": str(_device),
                    "device_name": torch.cuda.get_device_name(0) if torch.cuda.is_available() else "CPU",
                    "trained_on": _config.get("trained_on", "unknown"),
                    "vocab_size": _config.get("vocab_size", 50257),
                })
            else:
                self._send_json(503, {"status": "loading"})
        elif self.path == "/metrics":
            self._send_json(200, _config)
        else:
            self._send_json(404, {"error": "Not found"})

    def do_POST(self):
        if self.path == "/generate":
            if _model is None or _tokenizer is None:
                self._send_json(503, {"error": "Model is still initializing"})
                return

            content_len = int(self.headers.get("Content-Length", 0))
            raw_body = self.rfile.read(content_len).decode("utf-8")
            try:
                req = json.loads(raw_body)
            except Exception as e:
                self._send_json(400, {"error": f"Invalid JSON payload: {e}"})
                return

            prompt = req.get("prompt", "")
            max_new_tokens = int(req.get("max_new_tokens", 50))
            temperature = float(req.get("temperature", 0.8))
            top_k = int(req.get("top_k", 40))

            t0 = time.time()
            input_ids = _tokenizer.encode(prompt, return_tensors="pt").to(_device)

            with torch.no_grad():
                out_ids = _model.generate(
                    input_ids,
                    max_new_tokens=max_new_tokens,
                    temperature=temperature,
                    top_k=top_k,
                )

            generated_text = _tokenizer.decode(out_ids[0].tolist(), skip_special_tokens=True)
            latency_ms = round((time.time() - t0) * 1000, 2)

            self._send_json(200, {
                "prompt": prompt,
                "text": generated_text,
                "latency_ms": latency_ms,
                "device": str(_device),
                "device_name": torch.cuda.get_device_name(0) if torch.cuda.is_available() else "CPU",
                "trained_on": _config.get("trained_on", "unknown"),
            })
        else:
            self._send_json(404, {"error": "Not found"})


def run_server():
    load_model_from_gcs()
    server = ThreadingHTTPServer(("0.0.0.0", PORT), InferenceHandler)
    print(f"[Serving] Server listening on http://0.0.0.0:{PORT} ...", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("[Serving] Shutting down...", flush=True)
        server.server_close()


if __name__ == "__main__":
    run_server()
