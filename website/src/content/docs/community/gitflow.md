---
title: Gitflow
---

# Gitflow

## Branch model

Two long-lived branches:

| Branch | Environment | Access |
|---|---|---|
| `main` | Production | Protected — PR required |
| `develop` | Hors-prod (dev) | Protected — PR required |

## Feature workflow

```
develop
   │
   ├── feat/my-feature      ← branch from develop
   │        │
   │        └── PR → develop  ← code review + CI
   │
   ├── (PR merge)
   │
   └── release/vX.Y.Z       ← branch from develop when ready
            │
            └── PR → main   ← final QA, then tag
```

## Branch naming

| Type | Pattern | Example |
|---|---|---|
| Feature | `feat/<short-name>` | `feat/redis-metrics` |
| Bug fix | `fix/<short-name>` | `fix/kafka-timeout` |
| Release prep | `release/vX.Y.Z` | `release/v0.6.0` |
| Hotfix | `hotfix/<short-name>` | `hotfix/nil-pointer` |

## CI/CD matrix

| Event | CI | Dev deploy | Prod deploy |
|---|---|---|---|
| Push to `feat/*`, `fix/*` | Tests + vet | — | — |
| PR to `develop` | Tests + vet | — | — |
| Push to `develop` | Tests + vet | `dev` image | — |
| Push to `release/**` | Tests + vet | `rc-<sha>` image | — |
| Push to `main` | Tests + vet | — | — |
| Tag `gateway/vX.Y.Z` | Tests | — | `vX.Y.Z` image + GitHub Release |
| Tag `relay/vX.Y.Z` | Tests | — | `vX.Y.Z` image + GitHub Release |
| Helm chart change on `main` | — | — | Helm chart published to GitHub Pages |

## Common commands

```bash
# Start a feature
git checkout develop && git pull
git checkout -b feat/my-feature

# Open PR to develop
gh pr create --base develop --title "feat: ..."

# Create a release branch
git checkout develop && git pull
git checkout -b release/v0.6.0

# Open PR to main
gh pr create --base main --title "release: v0.6.0"
```

## Hotfixes

Hotfixes branch from `main` and are merged back to both `main` and `develop`:

```bash
git checkout main && git pull
git checkout -b hotfix/critical-fix

# fix, commit, PR → main
gh pr create --base main --title "fix: critical fix"

# after merge: back-merge to develop
git checkout develop && git pull
git merge --no-ff main
git push origin develop
```
