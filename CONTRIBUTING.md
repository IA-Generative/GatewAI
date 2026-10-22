# Contributing

## Gitflow

Ce repo suit un gitflow à deux branches permanentes.

```
main      → production
develop   → hors-prod (dev)
```

### Branches

| Branche | Rôle | Protection |
|---------|------|------------|
| `main` | Production — source de vérité pour les releases | PR + 1 review + CI obligatoire |
| `develop` | Hors-prod — intégration continue des features | PR + 1 review + CI obligatoire |
| `feat/*` | Nouvelle feature ou amélioration | Depuis `develop`, PR → `develop` |
| `fix/*` | Bug non critique | Depuis `develop`, PR → `develop` |
| `release/*` | Stabilisation avant livraison prod | Depuis `develop`, PR → `main` |
| `hotfix/*` | Correctif urgent en production | Depuis `main`, PR → `main` + back-merge `develop` |

### Flow standard

```
1. git checkout develop && git pull
2. git checkout -b feat/ma-feature
3. ... travail ...
4. PR → develop   (CI doit passer)
5. Validation sur hors-prod (image :dev déployée automatiquement)
6. Quand le lot est prêt pour la prod :
   git checkout -b release/gateway-v0.7.0 develop
   ... stabilisation uniquement (pas de nouvelle feature) ...
   PR → main
7. Sur main : /release   (tag + push + back-merge develop automatique)
```

### Flow hotfix

```
1. git checkout main && git pull
2. git checkout -b hotfix/gateway-v0.6.1
3. ... correctif ...
4. PR → main   (CI doit passer)
5. /release   (tag + back-merge develop automatique)
```

### Règles importantes

- **Les tags sont créés uniquement sur `main`**, jamais sur `develop`
- **Pas de commit direct** sur `main` ou `develop`
- **Les branches `feat/*` et `fix/*` vivent 2-3 jours max** — rebase sur `develop` si plus long
- **`release/*` = stabilisation uniquement** — aucune nouvelle feature, uniquement des correctifs de dernière minute

### CI par événement

| Événement | Action CI |
|-----------|-----------|
| Push ou PR sur `develop` | Tests + vet + helm lint |
| Push sur `develop` (merge) | Build image `:dev` → hors-prod |
| Push sur `release/**` (merge) | Build image `:rc-<sha>` → hors-prod |
| PR sur `main` | Tests + vet + helm lint |
| Tag `gateway/vX.Y.Z` sur `main` | Build image `:vX.Y.Z` + GitHub Release + Helm |
| Tag `relay/vX.Y.Z` sur `main` | Build image `:vX.Y.Z` + GitHub Release |
| Push sur `main` avec modifs `helm/**` | Publish Helm chart → GitHub Pages |

### Commandes utiles

```bash
# Démarrer une feature
git checkout develop && git pull
git checkout -b feat/ma-feature

# Préparer une release
git checkout -b release/gateway-v0.7.0 develop

# Lancer une release (depuis main après merge de la PR)
# Dans Claude Code :
/release gateway
```

Voir aussi les [runbooks opérationnels](https://ia-generative.github.io/GatewAI/manage/runbooks) pour les procédures opérationnelles.
