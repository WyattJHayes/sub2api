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
COPY wheelhouse /opt/radar-wheelhouse
RUN PIP_NO_INDEX=1 PIP_FIND_LINKS=/opt/radar-wheelhouse \
    python -m pip install --no-cache-dir --require-hashes -r requirements.lock \
    && rm -rf /opt/radar-wheelhouse
COPY pyproject.toml ./
COPY src ./src
RUN python -m pip install --no-cache-dir --no-deps .
RUN install -d -o radar-worker -g radar-worker /var/lib/sub2api-radar

USER radar-worker

ENTRYPOINT ["/usr/local/bin/radar-runner"]
