# Model Quality Radar Configuration

Model Quality Radar uses dedicated evaluation identities. Do not reuse a customer user, group, or API key. Apply migrations through `196_add_evaluation_key_events.sql` before enabling Radar. Migration `190_add_radar_route_evidence.sql` adds the evaluation-key flag and evidence table, migration `195_bind_evaluation_gateway_api_key.sql` freezes the dedicated key on each plan, and migration `196_add_evaluation_key_events.sql` adds the enablement audit trail.

## Server configuration

Set all six variables on every API server instance:

```bash
RADAR_ENABLED=true
RADAR_CONTEXT_SIGNING_KEY=<at-least-32-random-bytes>
RADAR_EVIDENCE_HASH_KEY=<different-at-least-32-random-bytes>
RADAR_REGION=cn-east
RADAR_ROUTE_PROFILE_VERSION=route-v42
RADAR_MAX_CONTEXT_TTL_SECONDS=300
```

`RADAR_CONTEXT_SIGNING_KEY` verifies evaluator-issued request contexts. `RADAR_EVIDENCE_HASH_KEY` creates stable HMAC references for account and channel IDs; it must be different from the signing key. Store both in the deployment secret manager, never in source control or evaluator output. The legacy names `RADAR_SIGNING_SECRET` and `RADAR_HASHING_SECRET` remain accepted, but the names above take precedence.

`RADAR_MAX_CONTEXT_TTL_SECONDS` must be between 1 and 900. `RADAR_REGION` and `RADAR_ROUTE_PROFILE_VERSION` are required when Radar is enabled and become evidence dimensions. Restart all API instances after changing these values. Rotating the signing key invalidates outstanding evaluation tokens; rotating the evidence hash key changes future redacted resource references.

## Provision an isolated identity

The examples use the existing API conventions. Replace the host, credentials, limits, and IDs for the deployment. Keep the nonzero limits: an unlimited evaluation key defeats isolation.

1. Create a dedicated exclusive group as an administrator:

```bash
curl -fsS -X POST "$SUB2API_URL/api/v1/admin/groups" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "model-quality-radar",
    "description": "Dedicated model quality evaluation traffic",
    "platform": "openai",
    "rate_multiplier": 1,
    "is_exclusive": true,
    "subscription_type": "standard",
    "rpm_limit": 10
  }'
```

Record the returned group ID as `RADAR_GROUP_ID`. Attach only the upstream accounts/channels intended for this route profile using the normal group/account administration APIs. Do not add customer keys to this group.

2. Create a dedicated user and grant only that group. User concurrency and RPM are independent safeguards:

```bash
curl -fsS -X POST "$SUB2API_URL/api/v1/admin/users" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d "{
    \"email\": \"radar-evaluator@example.invalid\",
    \"password\": \"$RADAR_USER_INITIAL_PASSWORD\",
    \"username\": \"radar-evaluator\",
    \"notes\": \"Dedicated model quality evaluation identity\",
    \"role\": \"user\",
    \"balance\": 10,
    \"concurrency\": 1,
    \"rpm_limit\": 10,
    \"allowed_groups\": [$RADAR_GROUP_ID]
  }"
```

Record the returned user ID as `RADAR_USER_ID`. Authenticate as this user through the deployment's normal sign-in flow and store the resulting JWT as `RADAR_USER_JWT`.

3. Create one key as the dedicated user. The API configures its group, quota, expiry, and rolling spend limits:

```bash
curl -fsS -X POST "$SUB2API_URL/api/v1/keys" \
  -H "Authorization: Bearer $RADAR_USER_JWT" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: radar-key-provision-v1" \
  -d "{
    \"name\": \"model-quality-radar\",
    \"group_id\": $RADAR_GROUP_ID,
    \"quota\": 10,
    \"expires_in_days\": 30,
    \"rate_limit_5h\": 1,
    \"rate_limit_1d\": 2,
    \"rate_limit_7d\": 5
  }"
```

Record the returned key ID as `RADAR_API_KEY_ID` and the generated key in the evaluator's secret manager. Do not send an inference request yet.

4. Grant the first global `platform_admin` Radar binding while the bootstrap rule is active. Bootstrap access exists only while the database has no enabled global Radar binding. The first binding closes that path automatically.

`platform_admin` is deliberately limited to role, route, and evaluation-key administration. It does not imply dataset, run, policy, baseline, or gate permissions. A staging administrator that owns dataset creation, plan creation, run execution, key administration, and gate smoke testing needs four global bindings on the same actor:

- `platform_admin` for role bindings and evaluation-key enablement
- `quality_admin` for dataset creation, dataset publication, plans, and policies
- `test_operator` for run start, retry, and worker administration
- `release_manager` for gate decisions, waivers, and release approval

Create each binding with an empty global scope. Create `platform_admin` first, then use its `role_manage` permission for the remaining bindings:

```bash
curl -fsS -X POST "$SUB2API_URL/api/v1/admin/radar/rbac/role-bindings" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d "{\"actor_id\":$RADAR_ADMIN_USER_ID,\"role\":\"platform_admin\",\"scope\":{}}"
```

Repeat the request for `quality_admin`, `test_operator`, and `release_manager`. Verify all four enabled rows before creating the dataset or plan. A second bootstrap attempt must return `403` unless the caller has `role_manage` through an explicit binding.

The four bindings on one actor do not satisfy baseline separation of duties. A complete baseline promotion test needs a second active administrator with the required Radar binding because the proposer cannot approve the same baseline. Quality approval and release approval must be recorded by eligible actors according to the deployment's separation policy; production uses different users for proposal, quality approval, and release approval.

Radar currently carries the authenticated `TenantID` from the request subject. Until the workspace membership mapping is enabled, the fallback tenant is the authenticated user ID, so a scoped role-binding request for another user is rejected by the repository. Configure the real workspace tenant mapping before enabling independent approval subjects; keep pre-provisioned bindings in place for staging while this mapping is absent.

5. Enable the dedicated key through the audited governance API or the `Enable for Radar` control in the Runs view:

```bash
curl -fsS -X POST "$SUB2API_URL/api/v1/admin/radar/evaluation-keys/$RADAR_API_KEY_ID/enable" \
  -H "Authorization: Bearer $ADMIN_JWT"
```

The operation requires `evaluation_key_manage`. It succeeds only when the key, owning user, and group are active, the key has remaining quota, and the key has not expired. Every successful enable action inserts an immutable `evaluation_key_events` row with the authenticated actor ID. Repeating the operation remains auditable.

Verify the final isolation state:

```sql
SELECT k.id, k.is_evaluation, k.status, k.quota, k.quota_used,
       k.rate_limit_5h, k.rate_limit_1d, k.rate_limit_7d,
       u.id AS user_id, u.concurrency, u.rpm_limit,
       g.id AS group_id, g.is_exclusive, g.rpm_limit
FROM api_keys AS k
JOIN users AS u ON u.id = k.user_id
JOIN groups AS g ON g.id = k.group_id
WHERE k.id = :radar_api_key_id;
```

Verify the audit record separately:

```sql
SELECT api_key_id, action, actor_id, created_at
FROM evaluation_key_events
WHERE api_key_id = :radar_api_key_id
ORDER BY created_at DESC;
```

## Paired evaluation plans

Every plan entry carries independent baseline and candidate inference configuration. A shared configuration is rejected because it cannot detect a route or model change.

```json
[
  {
    "route": "deepseek-chat",
    "baseline": {
      "route": "deepseek-chat-v1",
      "temperature": 0,
      "max_tokens": 256
    },
    "candidate": {
      "route": "deepseek-chat-v2",
      "temperature": 0,
      "max_tokens": 256
    }
  }
]
```

The plan also freezes `gateway_api_key_id`, `max_run_cost`, `daily_cost_limit`, and `max_concurrency`. Run creation reserves the estimated cost under a row lock. Assignment claims recheck plan enablement, key eligibility, and active concurrency before leasing work.

For the deterministic staging acceptance, use the following matrix. The top-level route identifies the comparison while each side freezes the gateway model alias used for inference:

```json
[
  {
    "route": "radar-synthetic-quality",
    "baseline": {
      "route": "radar-synthetic-baseline",
      "temperature": 0
    },
    "candidate": {
      "route": "radar-synthetic-candidate",
      "temperature": 0
    }
  }
]
```

Each synthetic case uses an OpenAI chat-completions prompt, a scalar exact-match expectation, and a relative gateway URL:

```json
{
  "prompt_spec": {
    "messages": [{"role": "user", "content": "What is the capital of France?"}]
  },
  "expected_spec": "Paris",
  "execution_spec": {"url": "/v1/chat/completions"},
  "grader_id": "exact",
  "grader_version": "v1"
}
```

Do not wrap the exact expectation in an object. The exact grader compares the normalized JSON value directly with the extracted response text. Absolute execution URLs are rejected before the evaluation API key can leave the worker.

## Evaluation requests

The evaluator signs a short-lived context bound to `RADAR_API_KEY_ID` and sends it in `X-Sub2API-Evaluation-Token` with the normal API-key credential. Claims include a unique run ID, sample ID, dataset version, expected public model alias, route profile, API key ID, issue time, and expiry time. The server generates the route trace ID; clients cannot supply it.

A normal key carrying evaluation headers is rejected. An evaluation key without a valid, unexpired token bound to that key is rejected before inference. Do not retry either rejection as ordinary traffic.

Route evidence is best-effort and contains routing identifiers only as HMAC references. It must never contain prompts, completions, hidden reasoning, credentials, raw account IDs, raw channel IDs, or arbitrary upstream error text. Operational access to `evaluation_route_evidence` should be restricted to the Radar reader and database administrators.
