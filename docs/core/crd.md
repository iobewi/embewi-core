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
```

| Champ | Requis | Description |
|-------|--------|-------------|
| `spec.nodeId` | Oui | Identifiant unique du device. Doit correspondre exactement à `EMBEWI_NODE_ID` compilé dans le firmware. C'est la clé de réconciliation entre le heartbeat (`node_id`) et l'objet K8s. |

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
| `ip` | heartbeat (RemoteAddr) | IP de management du device — utilisée par l'EndpointSlice |
| `state` | heartbeat | État agent : `booting` `pending_verify` `running` `degraded` `rollback` `failed` |
| `firmwareDigest` | heartbeat | `sha256:<hex>` du firmware courant |
| `deploymentId` | heartbeat | ID du déploiement actuellement validé |
| `otaValidated` | heartbeat | `true` uniquement après `mark_valid` sur le device |
| `heapFree` | heartbeat | RAM libre en octets |
| `rssi` | heartbeat | Signal WiFi en dBm |
| `chip` / `idfVersion` / `flashSize` / `ramSize` | GET /info | Capacités hardware (peuplées au premier contact OTA) |
| `appPort` | GET /info | Port du service applicatif ESP |
| `ready` | calculé | `true` ssi `state==running && otaValidated && heartbeat<30s` |
| `lastHeartbeat` | heartbeat | Timestamp du dernier heartbeat reçu |

### Conditions

| Condition | `reason` | `status` | Déclencheur |
|-----------|----------|----------|-------------|
| `Provisioned` | `ProvisioningComplete` | True | node_id + token établis |
| `Provisioned` | `ProvisioningPending` | False | premier boot, portail AP actif |
| `Ready` | `HeartbeatOK` | True | heartbeat reçu < 2× période |
| `Ready` | `HeartbeatTimeout` | False | aucun heartbeat > seuil |
| `Ready` | `NotProvisioned` | Unknown | jamais enrôlé |

### Colonnes kubectl

```
kubectl get mcu
NAME               NODE ID              IP              STATE     VERSION   READY   AGE
esp32-motor-left   esp32-motor-left     192.168.10.50   running   1.0.0     true    2h
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
```

| Champ | Requis | Description |
|-------|--------|-------------|
| `spec.nodeName` | Non¹ | Pin explicite sur un McuNode par nom K8s |
| `spec.nodeSelector` | Non¹ | Sélection par labels — doit résoudre exactement 1 McuNode |
| `spec.firmware.image` | Oui | Référence OCI complète (`registre/repo:tag`) |
| `spec.firmware.name` | Non | Nom du firmware pour matching avec l'agent |
| `spec.firmware.version` | Non | Version déclarée |

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
| `Progressing` | `OTAInProgress` | True | phase Writing ou Confirming |
| `Progressing` | `DeploymentComplete` | False | OTA terminé, firmware stable |
| `Progressing` | `OTAFailed` | False | rollback ou état failed |
| `Available` | `WorkloadReady` | True | EndpointSlice.ready=true |
| `Available` | `PendingVerification` | False | state=pending_verify |
| `Available` | `DeviceDegraded` | False | state ∈ {degraded, rollback, failed} |
| `Available` | `HeartbeatTimeout` | False | device plus joignable |

`Ready` synthétique : `Ready=True` ← `Progressing=False` ET `Available=True`.

### Colonnes kubectl

```
kubectl get mcudep
NAME                       NODE               IMAGE                                          PHASE     AGE
wheel-controller-v1-1-0   esp32-motor-left   registry.local/embewi/wheel-controller:v1.1.0  Deployed  5m
```

---

## Ressources créées automatiquement

Pour chaque McuNode avec une IP connue, le controller crée dans le même namespace :

| Ressource | Nom | Contenu |
|-----------|-----|---------|
| `Service` | `embewi-<node-name>` | Port `status.appPort` (défaut 8080), selectorless |
| `EndpointSlice` | `embewi-<node-name>` | IP ESP, `ready=status.ready` |

Ces ressources sont réconciliées à chaque heartbeat. Supprimer le McuNode ne les
supprime pas automatiquement en MVP.
