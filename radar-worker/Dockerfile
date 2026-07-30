FROM sub2api/radar-worker:staging AS runtime

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
COPY src ./src

# The cached staging image already contains the pinned Python dependency set.
# Reuse it and only overlay the current source tree for fast, offline builds.
ENV PYTHONPATH=/opt/radar-worker/src

USER radar-worker

ENTRYPOINT ["/usr/local/bin/radar-runner"]
