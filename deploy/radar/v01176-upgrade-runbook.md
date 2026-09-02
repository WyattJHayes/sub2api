# Sub2API v0.1.176 Radar Upgrade Runbook

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

The candidate control-plane and Worker digests must each differ from the
retained current v0.1.173 production rollback digests. Mutable tags are
suitable for local builds only.

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
After the controlled merge and local verification are committed, calculate the
canonical 64-character source SHA256 and run the recorder from the repository
root. Its first 40 characters are the revision. The timestamp embedded in
`--image-tag` must match `--date`.

```bash
deploy/radar/build_v01176_ghcr.py \
  --version 0.1.176 \
  --image-tag 0.1.176-radar-v14-YYYYMMDDTHHMMSSZ \
  --commit <40 lowercase hex> \
  --source-sha256 <64 lowercase hex> \
  --date YYYY-MM-DDTHH:MM:SSZ \
  --node-image node:24-alpine@sha256:<64 lowercase hex> \
  --golang-image golang:1.26.5-alpine@sha256:<64 lowercase hex> \
  --alpine-image alpine:3.20@sha256:<64 lowercase hex> \
  --worker-python-base-image python:3.14-slim@sha256:<64 lowercase hex> \
  --push \
  --output /secure/release/radar-v01176-image-record.json
```

The recorder publishes only the two approved private repositories, pulls each
result by manifest digest, verifies its runtime version, and creates the JSON
record at mode `0600`. It refuses an existing output path and never records
Docker credentials or environment values.

## Nanjing Isolation Runner Binding

Run the prerelease gates only on the Tencent Cloud Nanjing isolation host. The
mode-`0600` Runner identity must bind the rehearsal `run_id`, the local
`/etc/machine-id` SHA256, and public IP `1.13.161.130`. Pass that public IP
explicitly; the preflight has no Runner IP default and rejects a missing value.
Do not reuse an identity issued for the old Runner at `101.43.35.235`.

```bash
export RADAR_LOCAL_RUNNER_IDENTITY=/secure/rehearsal/runner-identity.json
export RADAR_LOCAL_EXPECTED_RUNNER_IP=1.13.161.130
```

The `public_ip` field inside `RADAR_LOCAL_RUNNER_IDENTITY` must exactly match
`RADAR_LOCAL_EXPECTED_RUNNER_IP`. A mismatch, an expired identity, a different
machine fingerprint, or a file mode other than `0600` stops preflight before
Docker is used.

## Disposable Migration Rehearsal

Create a custom-format PostgreSQL backup outside `/opt/sub2api`, verify its
SHA256, and put the rehearsal environment file at mode `0600`. The controlled
source contains 284 source migration SQL files. Restoring the 285-record
production backup and applying `225_group_model_pricing.sql` must produce 286
runtime schema_migrations records.

Place the PostgreSQL password, application database password, and pgpass entry
in three separate non-symlink files in a mode-`0700` private directory; each
file must have mode `0600`. The rehearsal environment file must not contain a
password value. Export only the file paths, candidate bindings, and recorded
current-production rollback bindings before invoking the helper:

```bash
export RADAR_MIGRATION_REHEARSAL_BACKUP=/secure/rehearsal/sub2api.dump
export RADAR_MIGRATION_REHEARSAL_BACKUP_SHA256=<64 lowercase hex>
export RADAR_MIGRATION_REHEARSAL_ENV_FILE=/secure/rehearsal/radar.env
export RADAR_MIGRATION_REHEARSAL_PROJECT_NAME=sub2api-radar-v14-rehearsal
export RADAR_MIGRATION_REHEARSAL_DATABASE_HOST=sub2api-radar-v14-rehearsal-postgres
export RADAR_MIGRATION_REHEARSAL_POSTGRES_PASSWORD_FILE=/secure/rehearsal/postgres-password
export RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD_FILE=/secure/rehearsal/database-password
export RADAR_MIGRATION_REHEARSAL_PGPASS_FILE=/secure/rehearsal/database.pgpass
export RADAR_CONTROL_PLANE_IMAGE=registry.example/sub2api/radar-control-plane@sha256:<candidate>
export RADAR_CONTROL_PLANE_IMAGE_DIGEST=sha256:<candidate>
export RADAR_WORKER_IMAGE=registry.example/sub2api/radar-worker@sha256:<candidate-worker>
export RADAR_WORKER_IMAGE_DIGEST=sha256:<candidate-worker>
# These legacy variable names are compatibility-only. Their values must be the
# recorded current v0.1.173 production rollback images, pinned by digest.
export RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE=registry.example/sub2api/radar-control-plane@sha256:<current-production-control-plane>
export RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE_DIGEST=sha256:<current-production-control-plane>
export RADAR_V10_ROLLBACK_WORKER_IMAGE=registry.example/sub2api/radar-worker@sha256:<current-production-worker>
export RADAR_V10_ROLLBACK_WORKER_IMAGE_DIGEST=sha256:<current-production-worker>
bash deploy/radar/rehearse-v01176-migrations.sh
```

The helper creates only resources whose names end in `-rehearsal`, restores the
backup into a fresh PostgreSQL volume, records the migration list before and
after candidate startup, restarts the candidate to prove idempotency, and boots
the retained current v0.1.173 production control plane against the forward
schema. It also runs the retained current production Worker with lifecycle
protocol version 2. The evidence directory contains migration listings and a
redacted v3 JSON summary. Require `status` to be `passed`,
`migration_count` to be `286`, both migration semantic flags to be `true`, and
`rollback_worker_probe_ok` to be `true`. A checksum mismatch, missing exact
filename, candidate health failure, rollback health failure, or Worker probe
failure stops the release.

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

## Production Maintenance Boundary

This procedure stops before production. Enter a separately authorized
maintenance window only after the source and image provenance checks, migration
rehearsal, isolated acceptance, browser workflows, Worker checks, restart
check, and image rollback rehearsal all have fresh passing evidence. Preserve
the recorded current v0.1.173 production control-plane and Worker digests as
the rollback pair. Image rollback does not reverse forward migrations;
database restoration remains a separate, explicitly authorized recovery
procedure.
