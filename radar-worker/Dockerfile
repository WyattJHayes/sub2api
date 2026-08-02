ARG RADAR_WORKER_BASE_IMAGE=sub2api/radar-worker:staging

FROM ${RADAR_WORKER_BASE_IMAGE} AS runtime

ARG APP_UID=10001
ARG APP_GID=10001

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /opt/radar-worker

USER root
RUN rm -rf /opt/radar-worker/src

COPY pyproject.toml ./

# The cached staging base predates the RFC 8785 signing dependency. Install
# this small, pinned addition while reusing the remaining dependency layer.
RUN python -m pip install --no-cache-dir "rfc8785>=0.1.4,<0.2"

COPY src ./src

# The cached staging image already contains the pinned Python dependency set.
# Reuse it and only overlay the current source tree for fast, offline builds.
ENV PYTHONPATH=/opt/radar-worker/src

USER radar-worker

ENTRYPOINT ["/usr/local/bin/radar-runner"]
