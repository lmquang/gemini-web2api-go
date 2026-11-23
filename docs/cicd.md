# Container builds and k3s updates

The Docker workflow runs on pushes to `main`, version tags, and manual dispatch.
It runs the full Go test suite with the race detector and `go vet` before
publishing Linux amd64 and arm64 images to
`ghcr.io/lmquang/gemini-web2api-go`. Publishing uses the workflow's
`GITHUB_TOKEN` with `packages: write`; no registry password belongs in source.

Main builds publish `main`, `latest`, and `sha-<full-commit-sha>` tags. The build
summary reports the immutable digest. Deploy by digest so mutable tags cannot
change the running release unexpectedly.

The GHCR package is public. Anonymous pull was verified from both the registry
and the k3s cluster; this deployment does not need a GHCR pull secret. The first
published release and its deployment checks are recorded in the
[deployment record](false-tool-quota-deployment.md).

`cookie/` is excluded from Git and Docker build context. Runtime databases,
environment files, and the Git directory are also excluded from Docker context.
Keep credentials in runtime secrets or private local files.

## Update k3s

GitHub builds the image; an operator updates k3s after reviewing the successful
run and image digest. Cluster credentials are not stored in this repository or
passed to the build workflow.

The production target is context `default`, namespace `gemini-bridge`, deployment
and container `gemini-web2api`. Before changing it, record the current image and
confirm readiness. Verify that the new GHCR image can be pulled by the cluster.
If the package is private, configure a GHCR pull secret before changing the
deployment; do not include registry tokens in command arguments or manifests.

```sh
kubectl --context default -n gemini-bridge set image deployment/gemini-web2api \
  gemini-web2api=ghcr.io/lmquang/gemini-web2api-go@sha256:<verified-digest>
kubectl --context default -n gemini-bridge rollout status deployment/gemini-web2api --timeout=180s
```

Change only the image. Preserve runtime settings, SQLite storage, and 9Router.
The current deployment uses one replica with `Recreate`: an update briefly stops
service and clears in-memory conversation heads. SQLite data persists. Verify
the running digest, readiness, API health, and unchanged settings after rollout.
If startup or health fails, restore the recorded previous image and verify it.

The [false tool quota evidence](false-tool-quota-recovery.md) records the fix's
limits. A successful image rollout does not close the original model bug.
