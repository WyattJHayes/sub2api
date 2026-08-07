# Radar v10 Release Closure Design

## 1. Goal

Close the six remaining Radar release risks with evidence that survives a rebuild, a process restart, and a production rollback review.

The six workstreams are source reproducibility, artifact cleanup log semantics, pricing fallback observability, Worker restart gates, disk capacity governance, and production promotion with rollback proof.

## 2. Current Evidence

The running staging control plane uses image ID `sha256:5de118fe8e0be180bb27e270a4a7f686b757083737853173ab828d459bdcb015`.

Its server binary SHA256 is `bfe1c72ef8f9d5f8a514f9a3df55d161af80522a0a7888dab1f031dc09d00a24`. Its embedded fallback pricing file SHA256 is `139de8a906ce61dc3f086ed394cd01b6c2110341054d7576dce4c4775f358569`.

The clean release worktree starts from commit `fd7a1304233fbd7454e0d2e43ca536a8c716b35d`. That commit contains 248 SQL migrations and lacks the complete outbox consumer implementation. The staging database contains 255 applied migrations and has processed 18 route evidence terminalization events with no pending events.

The source snapshot at `/opt/sub2api-builds/radar-candidate-heartbeat-20260802` is the v8 plus tenant-scoped outbox source authority. The two terminalization runtime files under `/opt/sub2api-builds/radar-outbox-e2e-20260801` are the final v10 log-level overlay. The final build metadata fixes these values:

```text
VERSION=radar-terminalization-pricing-security-v10-20260802
COMMIT=radar-candidate-heartbeat-v8-plus-terminalization
DATE=2026-08-02T12:36:00Z
GOOS=linux
CGO_ENABLED=0
tags=embed
```

## 3. Source Reconstruction Contract

Source reconstruction uses four ordered layers.

1. The committed Git baseline supplies repository identity and files absent from the release snapshot.
2. The candidate heartbeat snapshot replaces every overlapping source file and adds its new files.
3. The final terminalization overlay replaces exactly `evaluation_terminalization_runtime.go` and its test.
4. The test-consistency overlay replaces `wire_gen_test.go` and `radar_revision_pipeline_e2e_test.go` from the preserved release worktree. These files align tests with production interfaces already present in the candidate source and do not enter the release binary.

The import excludes `.git`, `.superpowers`, `.env*`, `node_modules`, `.venv`, `__pycache__`, generated frontend build metadata, compiled server binaries, `backend/docker.tmp`, AppleDouble files, and `*.pre-tenant-hotfix` files.

The first reconstructed commit preserves the running v10 behavior. Its Linux AMD64 release build must reproduce the running binary SHA256 before any new behavior is added. A mismatch blocks the release and must be explained at file, toolchain, or build-argument level.

## 4. Logging Semantics

Background jobs use severity as an operational signal.

* `DEBUG` means a successful poll selected no work.
* `INFO` means a successful poll performed work or a controlled fallback remains healthy.
* `WARN` means an external refresh failed while a verified local or embedded source continues serving.
* `ERROR` means the operation failed and requires retry, intervention, or produced incomplete state.

Artifact cleanup therefore records an empty successful poll at `DEBUG`. It records selected successful work at `INFO` and any returned error at `ERROR` with structured counters.

Pricing refresh records remote fetch failures at `WARN` when a loaded local source remains usable. It records an `ERROR` only when no usable pricing source exists.

## 5. Pricing Source Observability

The pricing service publishes a lock-protected snapshot with these fields:

```go
type PricingSourceSnapshot struct {
    Source          string
    LocalHash       string
    LoadedAt        time.Time
    SourceUpdatedAt time.Time
    LastRefreshAt   time.Time
    LastRefreshOK   bool
    FallbackTotal   uint64
}
```

`Source` is one of `remote`, `local`, or `embedded`. Logs include `pricing_source`, `local_hash`, `source_age_seconds`, and `fallback_total`. The existing metrics collector exports the same bounded-label state and a monotonic fallback counter. Model names, URLs, credentials, prompts, and response content never become metric labels.

## 6. Restart Delta Gate

A release observation captures a pre-observation state for the control plane, runner, grader, and statistics containers. Verification captures a second state and requires all of the following:

* every named container still exists;
* every health status is `healthy`;
* every current restart count equals its captured value;
* the container start timestamp did not move;
* no Radar HTTP 5xx or Worker error marker occurred in the observation interval;
* terminalization pending count remains zero;
* outbox processing continues to advance when test events are present.

Historical restart counts stay visible as diagnostic evidence. Only an increase inside the controlled observation fails this gate.

## 7. Disk Capacity Gate

The host gate blocks a release when root filesystem use exceeds 85 percent or free capacity falls below 10 GiB. It reports Docker image and build-cache usage without automatically deleting rollback assets.

The protected set always includes the staging image, the immediate rollback image, the previous known-good rollback image, all images referenced by running containers, and the unrelated `sub2api-custom` image. Cleanup requires an explicit image inventory and may remove only unreferenced, unlabeled build artifacts outside that protected set.

## 8. Promotion And Rollback

Promotion follows a fixed sequence.

1. Verify the committed source, tests, Linux build, image labels, and source manifest.
2. Run migration checks against a disposable database restored from the staging backup.
3. Capture the staging restart and capacity baseline.
4. Deploy the candidate to staging and complete the observation gate.
5. Create and checksum a production database backup.
6. Record the current production image digest and configuration hashes.
7. Promote by immutable image digest.
8. Run health, API, outbox, pricing, and artifact cleanup smoke checks.
9. Exercise rollback to the recorded digest, verify health, then restore the accepted candidate only after rollback evidence is complete.

Any failed step keeps the previously healthy image active. Database migrations in this release must remain forward compatible with the retained rollback images.

## 9. Completion Evidence

The release is complete only when one evidence bundle contains:

* the source commit and a clean Git status;
* the source file manifest and migration count;
* red and green test outputs for both behavior fixes;
* complete Go and Worker test results;
* Linux binary and pricing file hashes;
* disposable migration rehearsal output;
* restart baseline and final snapshots;
* disk gate output;
* staging and production image digests;
* backup SHA256 verification;
* promotion smoke output;
* rollback and post-rollback smoke output.
