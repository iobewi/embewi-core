# Changelog

Format inspiré de [Keep a Changelog](https://keepachangelog.com/fr/).

## [1.0.0] - 2026-07-28

Première version publique. Implémente le contrat `v1alpha1`
([`embewi`](https://github.com/iobewi/embewi)) côté Core, vérifié croisé
contre [`embewi-agent-esp`](https://github.com/iobewi/embewi-agent-esp).

### Ajouté

**Contrôleurs Kubernetes**
- `McuNode` : enrôlement, Service + EndpointSlice selectorless piloté par
  `heartbeat.ip` (jamais l'IP source TCP), conditions `Provisioned`/`Ready`.
- `McuDeployment` : cycle OTA complet (`Binding → Pulling → Preparing →
  Writing → Activating → Confirming → Deployed/Failed`), idempotence via
  `staged.state`, timeout négatif en confirmation.
- `McuConfigMap` : réconciliation config runtime (NVS agent) merge-on-key,
  limites 15/63 caractères validées avant push.

**Protocole Core ↔ Agent (contrat §1-§8b)**
- Rotation de token Bearer sans coupure (`previousToken`, rejoué
  automatiquement jusqu'à confirmation par heartbeat).
- Rotation de certificat TLS sans cycle OTA (`POST /tls/cert` + reboot),
  Secret `kubernetes.io/tls` — compatible cert-manager.
- Réconciliation du port applicatif (`POST /app/port`), sans reboot.
- Reboot à la demande via annotation `embewi.io/reboot-requested`.
- Détection de divergence `heartbeat.ip` / IP source TCP (Event
  `HeartbeatIPMismatch`).
- Canal de détresse NTP : `state` jamais promu `Ready` tant que l'horloge
  device n'est pas synchronisée (`reason=clock_unsynced`), sans jamais
  couper le heartbeat.
- Découverte de version d'API (`api_versions`, `GET /info`) avec repli sur
  `v1alpha1` si absent.
- `PUT /ota/write` chunké (64 KiB, `Content-Range`) avec retry par plage et
  resync `416`, remplace l'ancien transfert monolithique.
- Détection de `node_id` dupliqué entre plusieurs `McuNode` (Event
  `DuplicateNodeID`, refuse d'attacher un heartbeat ambigu).
- Idempotence config (`generation` vs `active_generation`) : détecte un
  crash Core entre `POST /config` et `POST /reboot`.
- Vérification de signature Ed25519 des images OCI (opt-in,
  `OCI_TRUSTED_PUBLIC_KEY`) + re-hash du digest du blob réellement streamé.
- Mapping complet des codes d'erreur stables (§4b) vers des Events
  Kubernetes.

**Observabilité**
- Métriques Prometheus `mcunode_*` (heap, RSSI, uptime, température,
  stack HWM, heartbeat timestamp, config generation, ota_validated),
  nettoyage de cardinalité à la suppression d'un `McuNode`.
- Streaming de logs `ESP_LOGx` via WebSocket (`wss://.../v1alpha1/logs`).

**Déploiement**
- Manifestes Kubernetes : CRD, RBAC (`ClusterRole` scopé), `Deployment`,
  `Service` heartbeat (NodePort) + `Service` metrics (ClusterIP),
  `ServiceMonitor`.
- CI GitHub Actions : build/vet/test/gofmt sur push et PR.
- Documentation Sphinx complète (architecture, contrôleurs, procédures
  opérationnelles).

### Corrigé

- CRD `mcunodes`/`mcudeployments` : `spec.tokenRef` et `spec.configMapRef`
  absents du schéma OpenAPI — un CRD structural sans
  `x-kubernetes-preserve-unknown-fields` les aurait silencieusement
  supprimés à l'admission sur un vrai cluster.
- RBAC : `secrets` ne portait que `get/list/watch`, alors que la
  confirmation de rotation de token (`clearPreviousToken`) nécessite
  `patch`.
- `ServiceMonitor` : namespace incohérent avec le reste des manifestes, et
  aucun `Service` n'exposait de port nommé `metrics` — le pipeline
  Prometheus n'avait rien à scraper malgré `/metrics` actif.
- Cible Makefile `registry` orpheline (référençait un manifeste supprimé
  par un revert antérieur, jamais nettoyée).
- Divers correctifs de revue de code sur le cycle OTA, l'authentification
  heartbeat et la gestion des `OwnerReference`.

### Sécurité

- Comparaison temps constant des tokens Bearer (`subtle.ConstantTimeCompare`).
- Vérification de signature Ed25519 des images firmware avant transfert OTA
  (opt-in, posture dev/MVP par défaut).
- Re-hash du digest du blob OCI réellement streamé, distinct du digest
  simplement déclaré par le manifeste.
