# Radar Migration 199 Staging Runbook

This procedure promotes the Radar evidence and revision pipeline to writer protocol 2. Run it first against staging with evaluation traffic and all claim loops disabled.

## Preconditions

- Record the deployment commit, immutable API and Worker image digests, and rollback image digests.
- Set `RADAR_DATABASE_URL` to the staging PostgreSQL database.
- Set `RADAR_MIGRATIONS_DIR` to the directory containing migrations 199, 200, and 201.
- Set `RADAR_WRITER_PROTOCOL_VERSION=2` on every API and Worker image. This release rejects any other configured value during startup.
- Give every API and Worker process a stable UUID in `RADAR_WRITER_INSTANCE_ID` and a bounded kind in `RADAR_WRITER_KIND`.
- Pause evaluation API traffic, replacement processing, new Runner, Grader, and Statistics claims, reapers, schedulers, and evaluation outbox consumers.
- Keep normal customer inference traffic on the established non-evaluation path only when it does not open transactions on the target database. Otherwise schedule a database-wide maintenance window.

The cutover script accepts `RADAR_PSQL_BIN` when the operator needs a pinned PostgreSQL client. It defaults to `psql`.

## Cutover

From the release artifact containing the target migrations, run:

```bash
backend/scripts/radar_migration_199_cutover.sh audit
backend/scripts/radar_migration_199_cutover.sh drain
backend/scripts/radar_migration_199_cutover.sh close
backend/scripts/radar_migration_199_cutover.sh migrate
backend/scripts/radar_migration_199_cutover.sh enforce
```

`close` checks all live non-migration writer sessions, assignment, grading, analysis, outbox and session lease counts, every external transaction on the target database, and advisory locks. A failed pre-close check leaves the database in `draining/audit`. Stop the remaining owner through its normal lifecycle and rerun `close`.

`migrate` accepts only two schema states. Migrations 199, 200, and 201 must all be absent or all be present with matching checksums. The absent case applies all three in one PostgreSQL transaction while the database is closed. A partial set or checksum mismatch exits with the database still `closed/audit`. The successful transaction raises `minimum_protocol_version` to 2.

`enforce` verifies all three checksums and protocol 2 before setting `closed/enforce`.

## New Image Handshake

Start the protocol 2 image with evaluation traffic and claim loops still disabled. From that image, register its configured identity:

```bash
backend/scripts/radar_migration_199_cutover.sh register
```

Repeat registration for each API and Worker pool that must be ready at reopen. Confirm the expected UUIDs, kinds, protocol version 2, image digests, and health checks in the deployment system. Do not record credentials or request content in the evidence bundle.

Run the acceptance test against an isolated database before reopening staging:

```bash
cd backend
go test ./internal/integration -run TestRadarRevisionPipelineE2E -count=1
```

Reopen only after at least one current protocol session is live and no old protocol session remains:

```bash
backend/scripts/radar_migration_199_cutover.sh reopen
```

The final state is `open/enforce` with minimum protocol 2. Enable a small synthetic evaluation Run first. Resume schedulers, reapers, outbox consumers, Statistics, Grader, replacement, Gateway evidence, and Runner traffic in that order while watching lease and dead-letter metrics.

## Failure And Rollback

- An `audit` failure stays `open/audit`.
- A `close` precondition failure stays `draining/audit`.
- Any migration, enforcement, registration, acceptance, or reopen failure after close stays closed.
- Keep `minimum_protocol_version=2`. Do not lower it during incident response.
- A rollback image must implement protocol 2. Register its stable identity while closed, run the acceptance test, then reopen explicitly.
- Database migration rollback is forward repair. Migrations 199 through 201 are append-compatible and remain applied.

## Evidence Record

Record UTC timestamps, deployment commit, image digests, migration checksums, protocol version, each phase output, rejected writer audit count, writer session and lease counts, transaction and advisory lock counts, acceptance command summaries, final state, and the synthetic Run ID. Exclude request bodies, model outputs, token values, signing material, credentials, and real account or channel identifiers.
