# Embewi Core — Documentation

**Embewi Core** est le contrôleur Kubernetes qui pilote des devices ESP32 via le
[contrat `v1alpha1`](https://github.com/iobewi/embewi). Il gère le cycle de vie
des firmwares (OTA), la configuration runtime et l'exposition des devices comme
endpoints routables dans le cluster.

```text
  Core (Kubernetes, Go)  ── v1alpha1 ──►  Agent (ESP32, ESP-IDF)
  réconcilie, pousse OTA                  reçoit, valide, exécute
```

## Core

```{toctree}
:maxdepth: 2
:caption: Core

Architecture & heartbeat server <core/architecture>
CRD — McuNode, McuDeployment <core/crd>
Contrôleurs <core/controllers>
Client OCI <core/oci>
API agent (Core → ESP) <core/agent-api>
Configuration <core/configuration>
Design interne <core/design>
```

## Utilisation

```{toctree}
:maxdepth: 2
:caption: Utilisation

Enrôler un device <usage/enroller-un-device>
Déployer un firmware <usage/deployer-un-firmware>
Opérations courantes <usage/operations>
```

## Dépôts & docs système

- **Contrat** — [`iobewi/embewi`](https://github.com/iobewi/embewi) :
  spec normative `v1alpha1` · [📖 doc](https://iobewi.github.io/embewi/).
- **Agent** — [`iobewi/embewi-agent-esp`](https://github.com/iobewi/embewi-agent-esp) :
  firmware ESP32/ESP-IDF · [📖 doc](https://iobewi.github.io/embewi-agent-esp/).
- **Core** — [`iobewi/embewi-core`](https://github.com/iobewi/embewi-core) :
  ce dépôt.
