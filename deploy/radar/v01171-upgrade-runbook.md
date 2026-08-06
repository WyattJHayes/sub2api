# Sub2API v0.1.171 Radar Upgrade Runbook

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

The candidate control-plane digest and the retained v10 rollback digest must be
different. Mutable tags are suitable for local builds only.

## Disposable Migration Rehearsal

Create a custom-format PostgreSQL backup outside `/opt/sub2api`, verify its
SHA256, and put the rehearsal environment file at mode `0600`. Export the
candidate and rollback variables before invoking the helper:

```bash
export RADAR_MIGRATION_REHEARSAL_BACKUP=/secure/rehearsal/sub2api.dump
export RADAR_MIGRATION_REHEARSAL_BACKUP_SHA256=<64 lowercase hex>
export RADAR_MIGRATION_REHEARSAL_ENV_FILE=/secure/rehearsal/radar.env
export RADAR_MIGRATION_REHEARSAL_PROJECT_NAME=sub2api-radar-v11-rehearsal
export RADAR_MIGRATION_REHEARSAL_DATABASE_HOST=sub2api-radar-v11-rehearsal-postgres
export RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD=<secret from the staging secret store>
export RADAR_CONTROL_PLANE_IMAGE=registry.example/sub2api/radar-control-plane@sha256:<candidate>
export RADAR_CONTROL_PLANE_IMAGE_DIGEST=sha256:<candidate>
export RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE=registry.example/sub2api/radar-control-plane@sha256:<v10>
export RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE_DIGEST=sha256:<v10>
deploy/radar/rehearse-v01171-migrations.sh
```

The helper creates only resources whose names end in `-rehearsal`, restores the
backup into a fresh PostgreSQL volume, records the migration list before and
after candidate startup, restarts the candidate to prove idempotency, and boots
the retained v10 image against the forward schema. The evidence directory
contains migration listings and a redacted JSON summary. A checksum mismatch,
missing exact filename, candidate health failure, or v10 health failure stops
the release.

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
checks before observation. Keep the previous v10 digests available.

On a hard-gate failure, roll back image references to the recorded v10 digests
and smoke test. Image rollback does not reverse forward migrations. Database
restoration is a separate, explicitly authorized recovery procedure.
