ARG RADAR_WORKER_PYTHON_BASE_IMAGE

FROM ${RADAR_WORKER_PYTHON_BASE_IMAGE} AS runtime

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

RUN groupadd --gid 10001 radar-worker \
    && useradd --uid 10001 --gid 10001 --create-home radar-worker
WORKDIR /opt/radar-worker
COPY requirements.lock ./
RUN python -m pip install --no-cache-dir --require-hashes -r requirements.lock
COPY pyproject.toml ./
COPY src ./src
RUN python -m pip install --no-cache-dir --no-deps .

USER radar-worker

ENTRYPOINT ["/usr/local/bin/radar-runner"]
