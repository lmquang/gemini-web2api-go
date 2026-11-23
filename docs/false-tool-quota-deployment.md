# False tool quota recovery release candidate

Status: prepared, not deployed. The bug remains OPEN. A fresh natural OpenCode
walkthrough and implementation flow passed on authenticated Neon after an earlier
attempt stopped on an actual upstream model downgrade. The original refusal has
not been reproduced. See [verification evidence](false-tool-quota-recovery.md).
Do not treat the image build or Kubernetes dry run as acceptance of the bug fix.

## Candidate

- Runtime source: commit `893576ee74e131126f0c6265e331bd0a8d5d210e`.
- Build: repository production Dockerfile, clean Git archive of that commit.
- Registry tag: `registry.chartify.xyz/gemini-web2api-go:quota-recovery-893576e`.
- Immutable image: `registry.chartify.xyz/gemini-web2api-go@sha256:d290b1c1b67e57ff266d35e872f6a8290692d70c3eecd44b150d4d074abe94db`.
- The image contains no local verification configuration, cookie export, private
  captures, or database. The isolated Docker test mounts its own config and data.

The registry manifest was read back by digest. Kubernetes accepted an image-only
server dry run. A subsequent live read still showed generation 6, one ready
replica, and the original image. No rollout occurred.

## Deployment scope

The proposed change updates only container `gemini-web2api` in deployment
`gemini-web2api`, namespace `gemini-bridge`, context `default`. It does not apply
the whole historical manifest or alter 9Router, secrets, storage, or settings.
Deployment requires explicit approval after review and the remaining live checks.

The inspected deployment has one replica and uses `Recreate`. Deployment or
rollback therefore causes a brief outage and loses the in-memory conversation
cache. The SQLite volume persists. Cached session identity is not durable.

The inspected production settings differ from isolated verification:
`strict_model=false`, `fallback_anon=true`, concurrency 5; the local checks use
`strict_model=true`, `fallback_anon=false`, concurrency 1. Both use multi-turn
and a 300 s request deadline. This image-only change preserves production
settings. Local strict-Neon evidence does not prove all production fallback paths.

## Approved rollout commands

These commands are a review plan, not authorization to run them. Before applying,
confirm that the deployment still uses the rollback image below and has no newer
change. Recheck health, runtime flags, and active traffic before the outage.

```sh
kubectl --context default -n gemini-bridge set image deployment/gemini-web2api \
  gemini-web2api=registry.chartify.xyz/gemini-web2api-go@sha256:d290b1c1b67e57ff266d35e872f6a8290692d70c3eecd44b150d4d074abe94db
kubectl --context default -n gemini-bridge rollout status deployment/gemini-web2api --timeout=180s
```

After approval and rollout, verify the running image digest, readiness, unchanged
runtime settings, authenticated model attribution, actual tool execution and a
tool-result continuation. Report failures without enabling anonymous fallback
or weakening model checks for the test. Keep model-prose refusals separate from
real provider errors. Readiness alone does not establish recovery.

Rollback to the image currently running at preparation time:

```sh
kubectl --context default -n gemini-bridge set image deployment/gemini-web2api \
  gemini-web2api=registry.chartify.xyz/gemini-web2api-go@sha256:0d29123dde4ba18058673fc6a99db1f9d6e0b5667145a821814c59fda1670119
kubectl --context default -n gemini-bridge rollout status deployment/gemini-web2api --timeout=180s
```

Rollback also uses `Recreate`; it cannot restore the old in-memory heads. Do not
replace a newer deployment with this historical snapshot.
