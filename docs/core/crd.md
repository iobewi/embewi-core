# CRD — Référence

## McuNode

Représente un device ESP physique. Le status est **entièrement piloté par les
heartbeats** — jamais édité manuellement.

### Spec

```yaml
apiVersion: embewi.io/v1alpha1
kind: McuNode
metadata:
  name: esp32-motor-left
  namespace: default
  labels:
    role: motor
    side: left
spec:
  nodeId: esp32-motor-left
  tokenRef:
    name: esp32-motor-left-token   # Secret K8s portant le token Bearer de ce device
    namespace: default             # optionnel — défaut = namespace du McuNode
```

| Champ | Requis | Description |
|-------|--------|-------------|
| `spec.nodeId` | Oui | Identifiant unique du device. Doit correspondre exactement à `EMBEWI_NODE_ID` compilé dans le firmware. C'est la clé de réconciliation entre le heartbeat (`node_id`) et l'objet K8s. |
| `spec.tokenRef.name` | Oui | Nom du Secret K8s contenant le token Bearer de ce device (§1 contrat). |
| `spec.tokenRef.namespace` | Non | Namespace du Secret. Défaut : namespace du McuNode. |

Le Secret référencé doit contenir **une clé `token`** dont la valeur est le token Bearer brut :

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: esp32-motor-left-token
  namespace: default
type: Opaque
stringData:
  token: "a3f7c1b2e8d09441f6bc3e7a2c504d8f"
```

### Status

```yaml
status:
  ip: "192.168.10.50"
  state: running
  firmwareName: wheel-controller
  firmwareVersion: "1.0.0"
  firmwareDigest: "sha256:a3f7c1..."
  deploymentId: "a1b2c3d4-..."
  otaValidated: true
  heapFree: 82344
  rssi: -61
  uptimeMs: 120034
  chip: esp32c3
  idfVersion: v6.0.0
  flashSize: 4194304
  ramSize: 401408
  appPort: 8080
  ready: true
  lastHeartbeat: "2026-06-16T10:30:00Z"
```

| Champ | Source | Description |
|-------|--------|-------------|
| `ip` | heartbeat | IP de management du device — source de vérité pour l'EndpointSlice (§8 contrat) |
| `state` | heartbeat | État agent : `booting` `pending_verify` `running` `degraded` `rollback` `failed` `offline` |
| `firmwareDigest` | heartbeat | `sha256:<hex>` du firmware courant |
| `deploymentId` | heartbeat | ID du déploiement actuellement validé |
| `otaValidated` | heartbeat | `true` uniquement après `mark_valid` sur le device |
| `heapFree` | heartbeat | RAM libre en octets |
| `rssi` | heartbeat | Signal WiFi en dBm |
| `chip` / `idfVersion` / `flashSize` / `ramSize` | GET /info | Capacités hardware — peuplées lors du premier contact OTA (`phasePreparing` / `phaseActivating`) |
| `appPort` | GET /info | Port du service applicatif ESP (utilisé par l'EndpointSlice) |
| `ready` | calculé | `true` ssi `state==running && otaValidated && heartbeat<10s` |
| `lastHeartbeat` | heartbeat | Timestamp du dernier heartbeat reçu |

### Conditions

| Condition | `reason` | `status` | Déclencheur |
|-----------|----------|----------|-------------|
| `Provisioned` | `ProvisioningComplete` | True | heartbeat reçu, node_id + token établis |
| `Provisioned` | `ProvisioningPending` | False | aucun heartbeat, device non encore connecté |
| `Ready` | `HeartbeatOK` | True | heartbeat reçu, `state=running && ota_validated` |
| `Ready` | `DeviceNotReady` | False | heartbeat reçu mais device pas encore prêt (`pending_verify`, etc.) |
| `Ready` | `HeartbeatTimeout` | False | aucun heartbeat depuis > 10 s |
| `Ready` | `NotProvisioned` | Unknown | jamais enrôlé |

### Colonnes kubectl

```
kubectl get mcunode
NAME               STATUS    AGE   VERSION
esp32-motor-left   running   2h    1.0.0

kubectl get mcunode -o wide
NAME               STATUS    AGE   VERSION   IP
esp32-motor-left   running   2h    1.0.0     192.168.10.50
```

---

## McuDeployment

Décrit un déploiement de firmware sur un device ESP. Piloté par une machine d'état.

### Spec

```yaml
apiVersion: embewi.io/v1alpha1
kind: McuDeployment
metadata:
  name: wheel-controller-v1-1-0
  namespace: default
spec:
  nodeName: esp32-motor-left           # pin explicite (recommandé)
  # nodeSelector:                      # alternative — doit résoudre exactement 1 node
  #   role: motor
  firmware:
    image: registry.local/embewi/wheel-controller:v1.1.0
    name: wheel-controller             # optionnel — extrait de l'image si absent
    version: "1.1.0"                   # optionnel
  configMapRef: wheel-left-gpio        # optionnel — McuConfigMap dans le même namespace
```

| Champ | Requis | Description |
|-------|--------|-------------|
| `spec.nodeName` | Non¹ | Pin explicite sur un McuNode par nom K8s |
| `spec.nodeSelector` | Non¹ | Sélection par labels — doit résoudre exactement 1 McuNode |
| `spec.firmware.image` | Oui | Référence OCI complète (`registre/repo:tag`) |
| `spec.firmware.name` | Non | Nom du firmware pour matching avec l'agent |
| `spec.firmware.version` | Non | Version déclarée |
| `spec.configMapRef` | Non | Nom d'un McuConfigMap à pousser sur le device avant l'OTA |

> ¹ `nodeName` ou `nodeSelector` — l'un des deux doit être fourni. `nodeName` est
> recommandé (pin explicite, pas d'ambiguïté).

### Status

```yaml
status:
  phase: Deployed
  boundNode: esp32-motor-left
  deploymentId: "a1b2c3d4-5e6f-..."
  digest: "sha256:b9e4f2..."
  size: 983040
  message: "déploiement confirmé par heartbeat"
```

| Champ | Description |
|-------|-------------|
| `phase` | Phase courante de la machine d'état |
| `boundNode` | Nom K8s du McuNode résolu |
| `deploymentId` | `string(dep.UID)` — clé d'idempotence transmise à l'agent |
| `digest` | `sha256:<hex>` du blob firmware (issu du manifeste OCI) |
| `size` | Taille en octets du blob (issu du manifeste OCI) |
| `message` | Dernier message d'état ou d'erreur lisible |

### Phases

```text
""          → Binding    (résolution du McuNode cible)
Binding     → Pulling    (résolution manifeste OCI → Digest + Size)
Pulling     → Preparing  (POST /ota/prepare, idempotent via staged §6)
Preparing   → Writing    (PUT /ota/write, stream OCI → ESP)
Writing     → Activating (POST /ota/activate + reboot)
Activating  → Confirming (attente heartbeat running + ota_validated)
Confirming  → Deployed   (confirmation reçue)
*           → Failed     (erreur terminale ou timeout)
```

### Conditions

| Condition | `reason` | `status` | Déclencheur |
|-----------|----------|----------|-------------|
| `Progressing` | `OTAInProgress` | True | phases Binding → Confirming |
| `Progressing` | `DeploymentComplete` | False | OTA terminé, firmware stable |
| `Progressing` | `OTAFailed` | False | rollback ou état failed |
| `Available` | `WorkloadReady` | True | phase Deployed, heartbeat OK |
| `Available` | `PendingVerification` | False | phase Confirming |
| `Available` | `DeviceDegraded` | False | state ∈ {degraded, rollback, failed} |
| `Available` | `HeartbeatTimeout` | False | device plus joignable (phase Deployed) |
| **`Ready`** | `DeploymentReady` | **True** | **`Progressing=False` ET `Available=True`** |
| `Ready` | `DeploymentNotReady` | False | OTA en cours ou device non disponible |

`Ready` est une condition **synthétique** : `kubectl wait mcudeployment/x --for=condition=Ready`
est la façon recommandée d'attendre la fin d'un déploiement.

### Colonnes kubectl

```
kubectl get mcudeployment
NAME                       NODE               IMAGE                                          PHASE     AGE
wheel-controller-v1-1-0   esp32-motor-left   registry.local/embewi/wheel-controller:v1.1.0  Deployed  5m
```

---

## McuConfigMap

Configuration runtime à pousser sur un device via `POST /v1alpha1/config`.

```yaml
apiVersion: embewi.io/v1alpha1
kind: McuConfigMap
metadata:
  name: wheel-left-gpio
  namespace: default
data:
  gpio_button: "9"
  gpio_ws2812: "48"
  ntp_server: "ntp.local"
```

| Contrainte | Valeur | Comportement si dépassée |
|-----------|--------|--------------------------|
| Longueur clé | ≤ 15 caractères | McuDeployment → `Failed/ConfigInvalid` |
| Longueur valeur | ≤ 63 caractères | McuDeployment → `Failed/ConfigInvalid` |
| Préfixe clé `_` | Réservé agent | Filtré silencieusement avant push |

Lorsqu'un `McuConfigMap` est modifié, tous les `McuDeployment` en phase `Deployed`
qui le référencent sont réconciliés automatiquement : la config est poussée et le
device reboot si le NVS diverge.

---

## Ressources créées automatiquement

Pour chaque McuNode avec une IP connue, le controller crée dans le même namespace :

| Ressource | Nom | Contenu |
|-----------|-----|---------|
| `Service` | `embewi-<node-name>` | Port `status.appPort` (défaut 8080), selectorless |
| `EndpointSlice` | `embewi-<node-name>` | IP ESP, `ready=status.ready` |

Ces ressources ont le McuNode comme **owner** (OwnerReference) — elles sont
supprimées automatiquement si le McuNode est supprimé.
