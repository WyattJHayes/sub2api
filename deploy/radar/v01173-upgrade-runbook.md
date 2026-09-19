# Sub2API v0.1.173 Radar Upgrade Runbook

This runbook is for an isolated rehearsal and trial. It does not change the
production Compose project or restore a production database.

## Source and Image Inputs

Bind every candidate to the exact source commit and two OCI image digests:

```text
RADAR_CONTROL_PLANE_IMAGE=registry.example/sub2api/radar-control-plane@sha256:<64 lowercase hex>
RADAR_CONTROL_PLANE_IMAGE_DIGEST=sha256:<64 lowercase hex>
RADAR_WORKER_IMAGE=registry.example/sub2api/radar-worker@sha256:<64 lowercase hex>
RADAR_WORKER_IMAGE_DIGEST=sha256:<64 lowercase hex>
```

The candidate control-plane digest and the retained v0.1.171/v11 rollback digest must be
different. Mutable tags are suitable for local builds only.

## Private GHCR Authentication and Candidate Build

Authenticate interactively before publishing. The local release credential
requires `write:packages`; the separate offshore credential requires only
`read:packages`. Paste each token at the password prompt. Do not put tokens in
shell history, command arguments, environment files, release evidence, or the
repository.

```bash
docker login ghcr.io -u a895411690
```

Resolve every build base to a full `name@sha256:<64 lowercase hex>` reference.
After Tasks 1 through 6 and source verification are committed, calculate the
canonical 64-character source SHA256 and run the recorder from the repository
root. Its first 40 characters are the revision. The timestamp embedded in
`--image-tag` must match `--date`.

```bash
deploy/radar/build_v01173_ghcr.py \
  --version 0.1.173 \
  --image-tag 0.1.173-radar-v13-YYYYMMDDTHHMMSSZ \
  --commit <40 lowercase hex> \
  --source-sha256 <64 lowercase hex> \
  --date YYYY-MM-DDTHH:MM:SSZ \
  --node-image node:24-alpine@sha256:<64 lowercase hex> \
  --golang-image golang:1.26.5-alpine@sha256:<64 lowercase hex> \
  --alpine-image alpine:3.20@sha256:<64 lowercase hex> \
  --worker-python-base-image python:3.14-slim@sha256:<64 lowercase hex> \
  --push \
  --output /secure/release/radar-v01173-image-record.json
```

The recorder publishes only the two approved private repositories, pulls each
result by manifest digest, verifies its runtime version, and creates the JSON
record at mode `0600`. It refuses an existing output path and never records
Docker credentials or environment values.

## Disposable Migration Rehearsal

Create a custom-format PostgreSQL backup outside `/opt/sub2api`, verify its
SHA256, and put the rehearsal environment file at mode `0600`. Export the
candidate and rollback variables before invoking the helper:

```bash
export RADAR_MIGRATION_REHEARSAL_BACKUP=/secure/rehearsal/sub2api.dump
export RADAR_MIGRATION_REHEARSAL_BACKUP_SHA256=<64 lowercase hex>
export RADAR_MIGRATION_REHEARSAL_ENV_FILE=/secure/rehearsal/radar.env
export RADAR_MIGRATION_REHEARSAL_PROJECT_NAME=sub2api-radar-v13-rehearsal
export RADAR_MIGRATION_REHEARSAL_DATABASE_HOST=sub2api-radar-v13-rehearsal-postgres
export RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD=<secret from the staging secret store>
export RADAR_CONTROL_PLANE_IMAGE=registry.example/sub2api/radar-control-plane@sha256:<candidate>
export RADAR_CONTROL_PLANE_IMAGE_DIGEST=sha256:<candidate>
export RADAR_WORKER_IMAGE=registry.example/sub2api/radar-worker@sha256:<candidate-worker>
export RADAR_WORKER_IMAGE_DIGEST=sha256:<candidate-worker>
# The rehearsal helper retains these legacy environment variable names. Their
# values must point to the current v0.1.171/v11 production rollback images.
export RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE=registry.example/sub2api/radar-control-plane@sha256:<v11>
export RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE_DIGEST=sha256:<v11>
export RADAR_V10_ROLLBACK_WORKER_IMAGE=registry.example/sub2api/radar-worker@sha256:<v11-worker>
export RADAR_V10_ROLLBACK_WORKER_IMAGE_DIGEST=sha256:<v11-worker>
deploy/radar/rehearse-v01171-migrations.sh
```

The helper creates only resources whose names end in `-rehearsal`, restores the
backup into a fresh PostgreSQL volume, records the migration list before and
after candidate startup, restarts the candidate to prove idempotency, and boots
the retained v0.1.171/v11 control plane against the forward schema. It also runs
the retained v0.1.171/v11 Worker with lifecycle protocol version 2. The evidence directory
contains migration listings and a redacted v2 JSON summary. Require `status`
to be `passed` and `rollback_worker_probe_ok` to be `true`. A checksum mismatch,
missing exact filename, candidate health failure, rollback health failure, or Worker
probe failure stops the release.

Use `RADAR_MIGRATION_REHEARSAL_DRY_RUN=1` to validate inputs and render the
Docker command plan without creating containers.

## Offshore Trial

Use a separate Compose project, network, database, Redis, object-store, ClamAV
volume, and Worker state volumes. Set `RADAR_IMAGE_PULL_POLICY=always` and
start with `--no-build` using the accepted digests. Permit the staging hostname
at the CAPTCHA provider before testing login. Keep the existing production
upstream and `radar.weihub.cloud` response unchanged.

The trial gate requires all containers healthy, three Workers heartbeating with
protocol version 2, zero restart increase, no Radar HTTP 5xx, no persistent
outbox backlog, one completed low-quota evaluation visible in the embedded
Radar and Model Health views, and a successful Turnstile login.

## Production Promotion and Rollback

Promotion requires accepted source and image evidence, a successful disposable
rehearsal, a fresh production backup with restore verification, and explicit
production authorization. Update only the established control-plane and Worker
image references, run `docker compose up -d --no-build`, and execute the smoke
checks before observation. Keep the previous v0.1.171/v11 digests available.

On a hard-gate failure, roll back image references to the recorded v0.1.171/v11 digests
and smoke test. Image rollback does not reverse forward migrations. Database
restoration is a separate, explicitly authorized recovery procedure.
