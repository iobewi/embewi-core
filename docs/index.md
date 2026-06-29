# Embewi Core — Documentation

**Embewi Core** est le contrôleur Kubernetes qui pilote des devices ESP32 via le
[contrat `v1alpha1`](https://github.com/iobewi/embewi). Il gère le cycle de vie
des firmwares (OTA), la configuration runtime et l'exposition des devices comme
endpoints routables dans le cluster.

```text
  Core (Kubernetes, Go)  ── v1alpha1 ──►  Agent (ESP32, ESP-IDF)
  réconcilie, pousse OTA                  reçoit, valide, exécute
```

## Architecture & contrôleurs

```{toctree}
:maxdepth: 2
:caption: Référence

Vue d'ensemble <embewi-core>
API CRD <embewi-api>
```

## Conception interne

```{toctree}
:maxdepth: 2
:caption: Design

Notes de conception <embewi-core-design>
```

## Dépôts & docs système

- **Contrat** — [`iobewi/embewi`](https://github.com/iobewi/embewi) :
  spec normative `v1alpha1` · [📖 doc](https://iobewi.github.io/embewi/).
- **Agent** — [`iobewi/embewi-agent-esp`](https://github.com/iobewi/embewi-agent-esp) :
  firmware ESP32/ESP-IDF · [📖 doc](https://iobewi.github.io/embewi-agent-esp/).
- **Core** — [`iobewi/embewi-core`](https://github.com/iobewi/embewi-core) :
  ce dépôt.

> Principe : le **contrat** (transverse, versionné) vit dans `iobewi/embewi` ;
> la doc **propre au Core** vit ici, avec le code.
