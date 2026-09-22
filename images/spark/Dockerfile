# Custom Spark 4.0.1 Image with Python 3.12, NumPy, Pandas, PyArrow, and GCS client
# Compatible with Kubeflow Workspaces codeserver-python and jupyterlab (Python 3.12)
#

FROM apache/spark:4.0.1

USER root

RUN apt-get update && \
    apt-get install -y --no-install-recommends software-properties-common curl ca-certificates && \
    add-apt-repository ppa:deadsnakes/ppa -y && \
    apt-get update && \
    apt-get install -y --no-install-recommends python3.12 python3.12-dev python3.12-venv && \
    curl -sS https://bootstrap.pypa.io/get-pip.py | python3.12 && \
    python3.12 -m pip install --no-cache-dir numpy pandas pyarrow google-cloud-storage pyspark==4.0.1 pyspark-connect==4.0.1 && \
    ln -sf /usr/bin/python3.12 /usr/bin/python3 && \
    ln -sf /usr/bin/python3.12 /usr/bin/python && \
    rm -rf /var/lib/apt/lists/*

ENV PYSPARK_PYTHON=/usr/bin/python3.12
ENV PYSPARK_DRIVER_PYTHON=/usr/bin/python3.12

USER 185
