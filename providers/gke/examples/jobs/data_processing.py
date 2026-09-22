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

"""Distributed Data Processing Stage (Stage 1 of the ML Demo) - Apache Spark.

This is the ETL stage, run as a real **Apache Spark** job on Kubernetes via the
Kubeflow Spark Operator. The notebook submits a `SparkApplication` custom
resource; the Spark Operator launches one driver pod and N executor pods, and
Spark distributes the preprocessing across the executors - the canonical
"big data" pattern, now just another workload on the cluster.

What this job does (Fashion-MNIST: 60k train / 10k test, 28x28 grayscale):
  * reads the raw IDX files (downloaded by the driver on first run),
  * builds a distributed DataFrame of (label, pixels[]) rows,
  * normalizes pixels to [0, 1] and one-hot encodes labels *in parallel*
    across Spark executors,
  * applies a light augmentation (random horizontal flip) to the train set,
  * repartitions into `NUM_SHARDS` partitions and writes one compressed .npz
    shard per partition to the shared ReadWriteMany volume.

Output layout on the shared volume (mounted at DATA_ROOT):
  /data/processed/train/shard-000.npz ... shard-NNN.npz
  /data/processed/test/test.npz
  /data/processed/_SUCCESS

This file runs as the Spark driver's `mainApplicationFile`. It requires PySpark,
which is present in the standard `spark` container images.

Environment variables (set on the SparkApplication driver):
  DATA_ROOT    - mount path of the shared volume (default /data)
  NUM_SHARDS   - number of output shards / target partitions (default 4)
"""

import gzip
import os
import shutil
import struct
import subprocess
import sys
import time
import urllib.request


def _ensure_numpy():
    try:
        import numpy  # noqa: F401
        return
    except ImportError:
        pass
    target = "/tmp/pydeps"
    os.makedirs(target, exist_ok=True)
    if target not in sys.path:
        sys.path.insert(0, target)
    subprocess.check_call(
        [sys.executable, "-m", "pip", "install", "--quiet",
         "--target", target, "numpy"]
    )

def _ensure_gcs():
    try:
        from google.cloud import storage  # noqa: F401
        return
    except ImportError:
        pass
    target = "/tmp/pydeps"
    os.makedirs(target, exist_ok=True)
    if target not in sys.path:
        sys.path.insert(0, target)
    subprocess.check_call(
        [sys.executable, "-m", "pip", "install", "--quiet",
         "--target", target, "google-cloud-storage"]
    )


_ensure_numpy()
_ensure_gcs()
import numpy as np

FASHION_MNIST_BASE = "https://storage.googleapis.com/tensorflow/tf-keras-datasets"
FILES = {
    "train_images": "train-images-idx3-ubyte.gz",
    "train_labels": "train-labels-idx1-ubyte.gz",
    "test_images": "t10k-images-idx3-ubyte.gz",
    "test_labels": "t10k-labels-idx1-ubyte.gz",
}


def download(url, dest):
    if os.path.exists(dest):
        return
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    for attempt in range(5):
        try:
            urllib.request.urlretrieve(url, dest)
            return
        except Exception as e:  # noqa: BLE001 - retry any transient network error
            print(f"  download attempt {attempt + 1} failed: {e}", flush=True)
            time.sleep(2 ** attempt)
    raise RuntimeError(f"Failed to download {url}")


def read_idx_images(path):
    with gzip.open(path, "rb") as f:
        magic, num, rows, cols = struct.unpack(">IIII", f.read(16))
        buf = f.read()
    return np.frombuffer(buf, dtype=np.uint8).reshape(num, rows * cols)


def read_idx_labels(path):
    with gzip.open(path, "rb") as f:
        magic, num = struct.unpack(">II", f.read(8))
        buf = f.read()
    return np.frombuffer(buf, dtype=np.uint8)


def run_etl(spark, bucket_name, raw_dir="/tmp/raw", num_shards=4):
    from pyspark.sql import functions as F
    from pyspark.sql.types import (
        ArrayType,
        FloatType,
        IntegerType,
        StructField,
        StructType,
    )
    from google.cloud import storage

    client = storage.Client()
    bucket = client.bucket(bucket_name)

    print(f"[driver] Spark {spark.version}, target shards={num_shards}", flush=True)

    # --- Driver downloads the raw dataset (small; done once). ---
    print("[driver] downloading raw Fashion-MNIST...", flush=True)
    for key, fname in FILES.items():
        download(f"{FASHION_MNIST_BASE}/{fname}", os.path.join(raw_dir, fname))

    train_images = read_idx_images(os.path.join(raw_dir, FILES["train_images"]))
    train_labels = read_idx_labels(os.path.join(raw_dir, FILES["train_labels"]))
    print(f"[driver] loaded {train_images.shape[0]} raw train examples", flush=True)

    # --- Build a distributed DataFrame: (label, pixels[784 uint8]). ---
    schema = StructType([
        StructField("label", IntegerType(), False),
        StructField("pixels", ArrayType(IntegerType()), False),
    ])
    rows = [
        (int(lbl), img.tolist())
        for img, lbl in zip(train_images, train_labels)
    ]
    df = spark.createDataFrame(rows, schema=schema).repartition(num_shards)

    # --- Parallel transforms executed across Spark executors via UDFs. ---
    def normalize_and_flip(pixels, do_flip):
        # Executor side needs numpy
        _ensure_numpy()
        import numpy as np
        arr = np.asarray(pixels, dtype=np.float32) / 255.0
        if do_flip:
            arr = arr.reshape(28, 28)[:, ::-1].reshape(-1)
        return arr.tolist()

    def to_one_hot(label):
        oh = [0.0] * 10
        oh[label] = 1.0
        return oh

    normalize_and_flip.__module__ = "__main__"
    to_one_hot.__module__ = "__main__"

    norm_udf = F.udf(normalize_and_flip, ArrayType(FloatType()))
    onehot_udf = F.udf(to_one_hot, ArrayType(FloatType()))

    processed = (
        df.withColumn("do_flip", F.rand(seed=42) < F.lit(0.5))
          .withColumn("features", norm_udf(F.col("pixels"), F.col("do_flip")))
          .withColumn("target", onehot_udf(F.col("label")))
          .select("features", "target")
    )

    # --- Write one .npz shard per Spark partition to the shared volume. ---
    def write_partition(index, part_iter):
        _ensure_numpy()
        _ensure_gcs()
        import numpy as np
        import time
        from google.cloud import storage
        feats, targs = [], []
        for r in part_iter:
            feats.append(r["features"])
            targs.append(r["target"])
        if not feats:
            return iter([(index, 0)])
        images = np.asarray(feats, dtype=np.float32)
        labels = np.asarray(targs, dtype=np.float32)
        tmp_path = f"/tmp/shard-{index:03d}-{int(time.time()*1000)}.npz"
        np.savez_compressed(tmp_path, images=images, labels=labels)
        
        client = storage.Client()
        bucket = client.bucket(bucket_name)
        blob = bucket.blob(f"processed/train/shard-{index:03d}.npz")
        blob.upload_from_filename(tmp_path)
        
        if os.path.exists(tmp_path):
            os.remove(tmp_path)
        return iter([(index, images.shape[0])])

    try:
        counts = (
            processed.rdd
            .mapPartitionsWithIndex(write_partition)
            .filter(lambda x: x[1] > 0)
            .collect()
        )
    except (NotImplementedError, AttributeError, Exception) as e:
        if "rdd is not implemented" in str(e) or "NOT_IMPLEMENTED" in str(e):
            def write_pandas_partition(pdf_iter):
                _ensure_numpy()
                _ensure_gcs()
                import numpy as np
                import pandas as pd
                import time
                from google.cloud import storage
                from pyspark import TaskContext
                
                ctx = TaskContext.get()
                part_id = ctx.partitionId() if ctx else int(time.time()*1000)
                
                feats_list, targs_list = [], []
                for pdf in pdf_iter:
                    if pdf.empty:
                        continue
                    feats_list.append(np.stack(pdf["features"].values).astype(np.float32))
                    targs_list.append(np.stack(pdf["target"].values).astype(np.float32))
                
                if not feats_list:
                    yield pd.DataFrame({"count": [0]})
                    return
                
                feats = np.concatenate(feats_list)
                targs = np.concatenate(targs_list)
                
                tmp_path = f"/tmp/shard-{part_id:03d}-{int(time.time()*1000)}.npz"
                np.savez_compressed(tmp_path, images=feats, labels=targs)
                
                client = storage.Client()
                bucket = client.bucket(bucket_name)
                blob = bucket.blob(f"processed/train/shard-{part_id:03d}.npz")
                blob.upload_from_filename(tmp_path)
                if os.path.exists(tmp_path):
                    os.remove(tmp_path)
                
                yield pd.DataFrame({"count": [len(feats)]})

            write_pandas_partition.__module__ = "__main__"
            res = processed.mapInPandas(write_pandas_partition, "count int").collect()
            counts = [(i, r["count"]) for i, r in enumerate(res) if r["count"] > 0]
        else:
            raise

    total = sum(c for _, c in counts)
    for idx, c in sorted(counts):
        print(f"[executor] shard-{idx:03d}.npz -> {c} examples", flush=True)
    print(f"[driver] wrote {total} train examples across {len(counts)} shards", flush=True)

    # --- Test set: small, processed on the driver. ---
    test_images = read_idx_images(os.path.join(raw_dir, FILES["test_images"]))
    test_labels = read_idx_labels(os.path.join(raw_dir, FILES["test_labels"]))
    test_x = test_images.astype(np.float32) / 255.0
    test_y = np.zeros((test_labels.shape[0], 10), dtype=np.float32)
    test_y[np.arange(test_labels.shape[0]), test_labels] = 1.0
    tmp_test = "/tmp/test.npz"
    np.savez_compressed(tmp_test, images=test_x, labels=test_y)
    blob = bucket.blob("processed/test/test.npz")
    blob.upload_from_filename(tmp_test)
    if os.path.exists(tmp_test):
        os.remove(tmp_test)
    print(f"[driver] wrote {test_x.shape[0]} test examples", flush=True)

    tmp_success = "/tmp/_SUCCESS"
    with open(tmp_success, "w") as f:
        f.write(f"num_shards={len(counts)}\ntrain_examples={total}\n")
    blob = bucket.blob("processed/_SUCCESS")
    blob.upload_from_filename(tmp_success)
    if os.path.exists(tmp_success):
        os.remove(tmp_success)
    print("[driver] data processing complete: wrote _SUCCESS marker", flush=True)


def main():
    from pyspark.sql import SparkSession
    bucket_name = os.environ.get("GCS_BUCKET")
    if not bucket_name:
        raise ValueError("GCS_BUCKET environment variable must be set")
    num_shards = int(os.environ.get("NUM_SHARDS", "4"))
    
    spark = SparkSession.builder.appName("fashion-mnist-etl").getOrCreate()
    try:
        run_etl(spark, bucket_name=bucket_name, num_shards=num_shards)
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
