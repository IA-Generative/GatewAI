---
title: Releasing
---

# Releasing

## Pre-flight checklist

- [ ] On branch `release/vX.Y.Z`, PR merged to `main`
- [ ] `CHANGELOG.md` updated with the new version
- [ ] Helm `helm/gateway/Chart.yaml` version bumped (if chart changed)
- [ ] Helm `helm/gateway/values.yaml` `image.tag` updated
- [ ] `k8s/deployment-transcription.yaml` relay image tag updated (if relay changed)

## Tag and release

```bash
# Ensure main is up-to-date
git checkout main && git pull

# Tag gateway
git tag gateway/vX.Y.Z
git push origin gateway/vX.Y.Z

# Tag relay (if changed)
git tag relay/vX.Y.Z
git push origin relay/vX.Y.Z
```

CI will automatically:

1. Build multi-arch Docker images
2. Push to `ghcr.io/ia-generative/gatewai/gateway:vX.Y.Z`
3. Create a GitHub Release with binaries

## Back-merge to develop

After the release tag is pushed:

```bash
git checkout develop && git pull
git merge --no-ff main
git push origin develop
```

## Image locations

| Component | Registry |
|---|---|
| Gateway | `ghcr.io/ia-generative/gatewai/gateway:vX.Y.Z` |
| Relay | `ghcr.io/ia-generative/gatewai/relay:vX.Y.Z` |
| Dev (gateway) | `ghcr.io/ia-generative/gatewai/gateway:dev` |
| Dev (relay) | `ghcr.io/ia-generative/gatewai/relay:dev` |

## Helm chart

The Helm chart is published automatically when `helm/**` changes on `main` (via `chart-releaser-action`). The chart registry is at:

```
https://ia-generative.github.io/GatewAI
```

```bash
helm repo add GatewAI https://ia-generative.github.io/GatewAI
helm repo update
helm search repo GatewAI
```
