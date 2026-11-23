# False tool quota recovery deployment

Status: deployed to k3s with the user's explicit request. The bug remains OPEN;
see [verification evidence](false-tool-quota-recovery.md). Image deployment and
successful normal tool turns do not reproduce the original false refusal.

## Published image

- Source commit: `39998bbf1ead829de3b9469f268b89210b1e9c1a` on `main`.
- [Successful GitHub Actions run](https://github.com/lmquang/gemini-web2api-go/actions/runs/45198827423).
- Checks: full Go race tests, `go vet`, and Linux amd64/arm64 container builds.
- Public image: `ghcr.io/lmquang/gemini-web2api-go`.
- Immutable deployed image: `ghcr.io/lmquang/gemini-web2api-go@sha256:280357072f055de3c8b9228917788934af4d7bc50ef7a6b11ec9665bcb52d26d`.
- Registry labels confirm the source commit and repository. Anonymous registry
  pull succeeded; a temporary cluster pod pulled the image and ran `--version`
  successfully before rollout. The preflight pod was then deleted.

[CI configuration](cicd.md) describes future builds on main pushes. Runtime
cookies, databases, environment files, and Git metadata are excluded from Docker
context; `cookie/` is also ignored by Git.

The earlier local candidate built from `893576e`, published only to
`registry.chartify.xyz`, was not the image deployed in this rollout. The GHCR
build also includes the newer cookie-import changes merged from `origin/main`.

## Deployment result

The image-only update targeted context `default`, namespace `gemini-bridge`,
deployment and container `gemini-web2api`. Generation advanced from 6 to 7.
Rollout completed; pod `gemini-web2api-85ccbcdf99-xjwkl` was Ready with zero
restarts. Its reported image ID matched the deployed GHCR digest.

A hash comparison confirmed the deployment spec, excluding container images,
stayed unchanged. Admin config and `/v1/models` returned HTTP 200; the latter
listed 22 models. Runtime settings matched the earlier production snapshot:
`multi_turn=true`, `strict_model=false`, `fallback_anon=true`, `anon_first=false`,
`web_channel=false`, timeout 300 s, concurrency 5, prompt limit 128000 bytes,
and keepalive 0. These differ from strict isolated verification settings and
were preserved during deployment.

SQLite storage, secrets, and 9Router were not changed. The source manifest at
`re-agy/deploy/k3s/gemini-web2api.yaml` was updated only on its image line.
The deployment uses one replica and `Recreate`; rollout briefly stops service
and clears in-memory conversation heads. SQLite data persists.

## Production tool smoke check

A nonstreaming chat request supplied a single `read_file` client tool and asked
for a verification code stored in a temporary file. The model did not receive
that code in its prompt. The first response contained exactly one valid call
for the permitted file. The client harness read the actual file, supplied the
result using the returned tool-call ID, and required the exact code in the final
answer with no further tool call. This check passed.

The corresponding production request records were:

| Request | Upstream model | Account | Status | Duration |
| --- | --- | --- | --- | --- |
| 64 | 4 Neon | 3 | 200 | 185.797 s |
| 65 | 4 Neon | 3 | 200 | 23.994 s |

Attribution used the sole new matching request record after each call; it did
not infer the upstream model from the requested model name. Production settings
stayed unchanged, including their existing fallback policy. This establishes an
authenticated Neon tool-result continuation after rollout, not a live false-quota
recovery or reproduction of the affected historical sessions. Private response
captures and the structured verdict remain outside the repository.

## Rollback

The image immediately before this rollout was recorded as:

```text
registry.chartify.xyz/gemini-web2api-go@sha256:0d29123dde4ba18058673fc6a99db1f9d6e0b5667145a821814c59fda1670119
```

If rollback is needed, first confirm no newer deployment superseded this one.
Restore the image and the source manifest's image line together:

```sh
kubectl --context default -n gemini-bridge set image deployment/gemini-web2api \
  gemini-web2api=registry.chartify.xyz/gemini-web2api-go@sha256:0d29123dde4ba18058673fc6a99db1f9d6e0b5667145a821814c59fda1670119
kubectl --context default -n gemini-bridge rollout status deployment/gemini-web2api --timeout=180s
```

Rollback also uses `Recreate`; it cannot restore prior in-memory conversation
heads. Verify API health and unchanged runtime settings after any rollback.
