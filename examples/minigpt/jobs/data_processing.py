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

"""Distributed Data Processing Stage - Apache Spark on WikiText / Language Data.

This is the ETL stage, run as an Apache Spark job on Kubernetes using the Kubeflow
Spark SDK. It tokenizes text using the GPT-2 tokenizer, chunks tokens into sequences
of length `block_size + 1`, and saves compressed `.npz` training shards directly to GCS.
"""

import json
import os
import subprocess
import sys
import time
import urllib.request
import uuid

os.environ["HF_HOME"] = "/tmp/huggingface"


def _ensure_deps():
    try:
        import numpy  # noqa: F401
        from google.cloud import storage  # noqa: F401
        import pyarrow  # noqa: F401
        import transformers  # noqa: F401
        return
    except ImportError:
        pass
    target = "/tmp/pydeps"
    os.makedirs(target, exist_ok=True)
    if target not in sys.path:
        sys.path.insert(0, target)
    subprocess.check_call(
        [
            sys.executable,
            "-m",
            "pip",
            "install",
            "--quiet",
            "--target",
            target,
            "numpy",
            "google-cloud-storage",
            "pyarrow",
            "transformers",
        ]
    )


def run_etl(
    spark,
    bucket_name: str,
    num_shards: int = 4,
    num_records: int = 16000,
    block_size: int = 128,
):
    """Executes distributed ETL on Spark."""
    import pandas as pd
    import numpy as np

    print(f"[driver] Starting Spark ETL (shards={num_shards}, block_size={block_size})", flush=True)

    from google.cloud import storage
    client = storage.Client()
    bucket = client.bucket(bucket_name)

    # Clean up existing training shards in GCS
    blobs_to_delete = list(bucket.list_blobs(prefix="processed/"))
    if blobs_to_delete:
        print(f"[driver] Cleaning up {len(blobs_to_delete)} existing blobs under gs://{bucket_name}/processed/...", flush=True)
        bucket.delete_blobs(blobs_to_delete)

    df = spark.range(0, num_shards).repartition(num_shards)

    def process_shard(iterator):
        os.environ["HF_HOME"] = "/tmp/huggingface"
        _ensure_deps()
        import pandas as pd
        import numpy as np
        import uuid
        import urllib.request
        from google.cloud import storage
        from transformers import GPT2Tokenizer

        tokenizer = GPT2Tokenizer.from_pretrained("gpt2")
        base_url = "https://huggingface.co/datasets/JeanKaddour/minipile/resolve/refs%2Fconvert%2Fparquet/default/train"
        MAX_REMOTE_SHARDS = 12

        all_results = []

        for pdf in iterator:
            for shard_id in pdf["id"]:
                actual_id = shard_id % MAX_REMOTE_SHARDS
                url = f"{base_url}/{actual_id:04d}.parquet"
                tmp_parquet = f"/tmp/shard-{shard_id}.parquet"

                # Download parquet chunk on executor with fallback
                downloaded = False
                for attempt in range(3):
                    try:
                        req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
                        with urllib.request.urlopen(req, timeout=30) as resp, open(tmp_parquet, "wb") as f_out:
                            f_out.write(resp.read())
                        downloaded = True
                        break
                    except Exception as e:
                        print(f"[executor] Attempt {attempt+1} failed downloading {url}: {e}", flush=True)
                        time.sleep(2)

                texts = []
                if downloaded and os.path.exists(tmp_parquet):
                    try:
                        shard_df = pd.read_parquet(tmp_parquet)
                        if num_records and num_records > 0:
                            per_shard = max(1, num_records // num_shards)
                            shard_df = shard_df.iloc[:per_shard]
                        texts = [t for t in shard_df["text"] if isinstance(t, str) and t.strip()]
                    except Exception as e:
                        print(f"[executor] Failed to read parquet {tmp_parquet}: {e}", flush=True)
                    finally:
                        if os.path.exists(tmp_parquet):
                            os.remove(tmp_parquet)

                # Fallback to rich synthetic corpus if remote download fails or is rate-limited
                if not texts:
                    print(f"[executor] Generating fallback synthetic text corpus for shard {shard_id}", flush=True)
                    corpus_seeds = [
                        "Deep learning is a subset of machine learning based on artificial neural networks.",
                        "Cloud TPUs are Google's custom-developed application-specific integrated circuits (ASICs).",
                        "PyTorch is an open-source machine learning framework based on the Torch library.",
                        "Kubeflow is a free and open-source machine learning platform designed to enable ML pipelines on Kubernetes.",
                        "Large language models use transformer architectures to model complex sequences of text.",
                        "Distributed data parallel scales training across multiple GPUs and TPU cores synchronously.",
                    ]
                    texts = (corpus_seeds * 200)[:500]

                all_tokens = []
                for text in texts:
                    all_tokens.extend(tokenizer.encode(text))

                seq_len = block_size + 1
                num_blocks = len(all_tokens) // seq_len
                if num_blocks == 0:
                    continue

                all_tokens = all_tokens[: num_blocks * seq_len]
                data = np.array(all_tokens, dtype=np.uint16).reshape((num_blocks, seq_len))

                # inputs (x) and targets (y)
                inputs = data[:, :-1]
                targets = data[:, 1:]

                uid = str(uuid.uuid4())[:8]
                tmp_npz = f"/tmp/shard-{uid}.npz"
                np.savez_compressed(tmp_npz, inputs=inputs, targets=targets)

                client = storage.Client()
                b = client.bucket(bucket_name)
                blob = b.blob(f"processed/train/shard-{uid}.npz")
                blob.upload_from_filename(tmp_npz)

                if os.path.exists(tmp_npz):
                    os.remove(tmp_npz)

                res_df = pd.DataFrame({"shard": [uid], "count": [num_blocks]})
                res_df["shard"] = res_df["shard"].astype(str)
                res_df["count"] = res_df["count"].astype("int64")
                all_results.append(res_df)

        if not all_results:
            return iter([pd.DataFrame({"shard": pd.Series([], dtype="string"), "count": pd.Series([], dtype="int64")})])
        return iter(all_results)

    print("[driver] Launching distributed executors to process shards...", flush=True)
    counts_df = df.mapInPandas(process_shard, schema="shard string, count long")
    counts = counts_df.collect()

    total_blocks = sum(row["count"] for row in counts)
    for row in counts:
        print(f"[executor] shard-{row['shard']}.npz -> {row['count']} blocks uploaded to GCS", flush=True)
    print(f"[driver] Total {total_blocks} train blocks across {len(counts)} shards", flush=True)

    # Generate test shard on driver
    print("[driver] Generating test evaluation set...", flush=True)
    _ensure_deps()
    from transformers import GPT2Tokenizer
    tokenizer = GPT2Tokenizer.from_pretrained("gpt2")
    test_text = (
        "The development of machine learning systems requires careful consideration of data preprocessing, "
        "model architecture design, distributed training orchestration, and robust model serving. "
        "In this example, we verify full pipeline compatibility across both GPU and Cloud TPU hardware."
    ) * 50
    test_tokens = tokenizer.encode(test_text)
    seq_len = block_size + 1
    num_test_blocks = len(test_tokens) // seq_len
    if num_test_blocks > 0:
        test_tokens = test_tokens[: num_test_blocks * seq_len]
        test_data = np.array(test_tokens, dtype=np.uint16).reshape((num_test_blocks, seq_len))
        tmp_test = "/tmp/test.npz"
        np.savez_compressed(tmp_test, inputs=test_data[:, :-1], targets=test_data[:, 1:])
        b = client.bucket(bucket_name)
        blob = b.blob("processed/test/test.npz")
        blob.upload_from_filename(tmp_test)
        if os.path.exists(tmp_test):
            os.remove(tmp_test)
        print(f"[driver] Uploaded {num_test_blocks} evaluation blocks to gs://{bucket_name}/processed/test/test.npz", flush=True)

    # Write _SUCCESS marker
    marker_content = f"num_shards={len(counts)}\ntrain_examples={total_blocks}\nblock_size={block_size}\n"
    tmp_marker = "/tmp/_SUCCESS"
    with open(tmp_marker, "w") as f:
        f.write(marker_content)
    blob = bucket.blob("processed/_SUCCESS")
    blob.upload_from_filename(tmp_marker)
    if os.path.exists(tmp_marker):
        os.remove(tmp_marker)
    print(f"[driver] Spark ETL complete! Wrote _SUCCESS marker to gs://{bucket_name}/processed/_SUCCESS", flush=True)


def main():
    from pyspark.sql import SparkSession
    bucket_name = os.environ.get("DEMO_BUCKET") or os.environ.get("GCS_BUCKET")
    if not bucket_name:
        raise ValueError("DEMO_BUCKET or GCS_BUCKET environment variable must be set")
    num_shards = int(os.environ.get("NUM_SHARDS", "4"))
    num_records = int(os.environ.get("NUM_RECORDS", "16000"))
    block_size = int(os.environ.get("BLOCK_SIZE", "128"))

    spark = SparkSession.builder.appName("minigpt-spark-etl").getOrCreate()
    try:
        run_etl(spark, bucket_name=bucket_name, num_shards=num_shards, num_records=num_records, block_size=block_size)
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
