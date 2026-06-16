# Embewi Core — Référence technique

> Documentation du Runtime Core Go. Décrit exactement ce qui est implémenté dans
> `internal/controller/`, `internal/heartbeat/`, `internal/oci/` et `api/v1alpha1/`.
> Le contrat de référence reste `embewi-contract-v2.md`.

---

## Sommaire

1. [Architecture](#1-architecture)
2. [CRD McuNode](#2-crd-mcunode)
3. [CRD McuDeployment](#3-crd-mcudeployment)
4. [Heartbeat server](#4-heartbeat-server)
5. [McuNode reconciler](#5-mcunode-reconciler)
6. [McuDeployment reconciler](#6-mcudeployment-reconciler)
7. [Client OCI](#7-client-oci)
8. [Tokens d'authentification](#8-tokens-dauthentification)
9. [Configuration](#9-configuration)
10. [Opérations](#10-opérations)

---

## 1. Architecture

```
ESP32 Agent (HTTPS :443)              Embewi Core (Go)
┌─────────────────────┐              ┌────────────────────────────────────┐
│ GET  /v1alpha1/info │◄─────────────┤ McuDeploymentReconciler            │
│ POST /ota/prepare   │◄─────────────┤  Binding → Pulling →               │
│ PUT  /ota/write  ◄──┼──stream bin──┤  Preparing → Writing →             │
│ POST /ota/activate  │◄─────────────┤  Activating → Confirming →         │
│                     │              │  Deployed / Failed                  │
│ POST /v1alpha1/      │             ├────────────────────────────────────┤
│   heartbeat ────────┼────────────►│ heartbeat.Server                   │
│ POST /v1alpha1/logs─┼────────────►│  → patch McuNode.Status            │
└─────────────────────┘              ├────────────────────────────────────┤
                                     │ McuNodeReconciler                  │
         OCI Registry                │  → Service selectorless            │
┌─────────────────────┐              │  → EndpointSlice (IP ESP, ready)   │
│ manifest + blob ────┼────────────►│                                    │
└─────────────────────┘              ├────────────────────────────────────┤
                                     │ oci.Client                         │
         Secret K8s                  │  GET /v2/.../manifests/<tag>       │
┌─────────────────────┐              │  GET /v2/.../blobs/<digest>        │
│ embewi-tokens       │              └────────────────────────────────────┘
│  nodeId → token ────┼────────────► nodeClient()
└─────────────────────┘
```

**Deux flux :**
- **Inbound** (Core → ESP) : requêtes HTTPS initiées par le Core vers l'agent ESP.
- **Outbound** (ESP → Core) : heartbeats et logs initiés par l'agent vers le heartbeat server.

Le Core ne se connecte jamais directement à l'agent pour surveiller son état — il attend les heartbeats.

---

## 2. CRD McuNode

Représente un device ESP physique. Le status est **entièrement piloté par les heartbeats** — jamais édité manuellement.

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
  nodeId: esp32-motor-left   # doit correspondre à EMBEWI_NODE_ID compilé dans le firmware
```

| Champ | Requis | Description |
|-------|--------|-------------|
| `spec.nodeId` | Oui | Identifiant unique du device. Doit correspondre exactement à `EMBEWI_NODE_ID` dans le firmware ESP. C'est la clé de réconciliation entre le heartbeat (`node_id`) et l'objet K8s. |

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

### Colonnes affichées

```
kubectl get mcu
NAME               NODE ID              IP              STATE     VERSION   READY   AGE
esp32-motor-left   esp32-motor-left     192.168.10.50   running   1.0.0     true    2h
```

---

## 3. CRD McuDeployment

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

> ¹ `nodeName` ou `nodeSelector` — l'un des deux doit être fourni.

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

```
""          → Binding    (résolution du McuNode cible)
Binding     → Pulling    (résolution manifeste OCI → Digest + Size)
Pulling     → Preparing  (POST /ota/prepare, idempotent via staged §6)
Preparing   → Writing    (PUT /ota/write, stream OCI → ESP)
Writing     → Activating (POST /ota/activate + reboot)
Activating  → Confirming (attente heartbeat running + ota_validated)
Confirming  → Deployed   (confirmation reçue)
*           → Failed     (erreur terminale ou timeout)
```

### Colonnes affichées

```
kubectl get mcudep
NAME                       NODE               IMAGE                                          PHASE     AGE
wheel-controller-v1-1-0   esp32-motor-left   registry.local/embewi/wheel-controller:v1.1.0  Deployed  5m
```

---

## 4. Heartbeat server

Serveur HTTP simple (pas HTTPS — flux interne cluster) qui reçoit les flux sortants ESP.  
Adresse par défaut : `:8080`. Configuré via `--heartbeat-address`.

### `POST /v1alpha1/heartbeat`

Met à jour le `McuNode.Status` correspondant au `node_id` reçu.

**Corps attendu :**

```json
{
  "node_id":         "esp32-motor-left",
  "ts":              120034,
  "state":           "running",
  "deployment_id":   "a1b2c3d4-5e6f-...",
  "firmware_digest": "sha256:b9e4f2...",
  "ota_validated":   true,
  "uptime_ms":       120034,
  "heap_free":       82344,
  "rssi":            -61
}
```

**Comportement :**

1. Recherche le McuNode dont `spec.nodeId == node_id` (tous namespaces).
2. Extrait l'IP source depuis `RemoteAddr` (pas de champ IP dans le payload).
3. Patch `McuNode.Status` : ip, state, firmwareDigest, deploymentId, otaValidated, heapFree, rssi, uptimeMs, ready, lastHeartbeat.
4. Répond toujours **200** — même si le McuNode est inconnu (l'agent ne doit pas crasher sur un 404).

**`ready` calculé localement :**
```
ready = (state == "running") && ota_validated
```
Le McuNode reconciler affine ensuite avec le timeout heartbeat (30 s).

### `POST /v1alpha1/logs`

Reçoit les logs d'événements de l'agent. Pas de persistance — écriture dans le logger zap du controller.

**Corps attendu :**

```json
{
  "ts":       120034,
  "node":     "esp32-motor-left",
  "workload": "wheel-controller",
  "level":    "error",
  "msg":      "self-check KO, rollback"
}
```

| `level` | Log zap produit |
|---------|----------------|
| `fatal` | `logger.Error` |
| `error` | `logger.Error` |
| `info`  | `logger.Info`  |

Répond toujours **200**. Les entrées JSON malformées sont absorbées silencieusement.

---

## 5. McuNode reconciler

Déclenché à chaque modification d'un McuNode. Responsabilités :

### 5.1 Pilotage de `ready`

```
wantReady = state=="running" && otaValidated && time.Since(lastHeartbeat) ≤ 30s
```

Si `wantReady != status.ready` → patch status. La condition est réévaluée toutes les **30 s** via `RequeueAfter`.

Cas de passage à `ready=false` :
- Heartbeat silencieux depuis > 30 s
- `state` ≠ `running` (pending_verify, degraded, rollback, failed)
- `otaValidated == false`

### 5.2 Service selectorless

Créé ou mis à jour pour chaque McuNode ayant une IP :

```yaml
apiVersion: v1
kind: Service
metadata:
  name: embewi-<node-name>
  namespace: <namespace>
  labels:
    embewi.io/managed-by: embewi-controller
    embewi.io/node-id: esp32-motor-left
spec:
  ports:
    - name: app
      port: <status.appPort>    # défaut 8080
      protocol: TCP
  # Pas de selector — l'EndpointSlice est géré manuellement
```

### 5.3 EndpointSlice

```yaml
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: embewi-<node-name>
  labels:
    kubernetes.io/service-name: embewi-<node-name>
    embewi.io/managed-by: embewi-controller
addressType: IPv4
endpoints:
  - addresses: ["192.168.10.50"]    # status.ip
    conditions:
      ready: true                   # piloté par status.ready
ports:
  - name: app
    port: 8080
    protocol: TCP
```

`ready` suit `McuNode.Status.Ready` — jamais statique. C'est le seul moyen pour le cluster de savoir si l'ESP est opérationnel.

---

## 6. McuDeployment reconciler

Machine d'état pilotée par `Status.Phase`. Chaque appel à `Reconcile` exécute exactement une phase, puis requeue.

### 6.1 Phase Binding

Résout le McuNode cible selon `spec.nodeName` (prioritaire) ou `spec.nodeSelector`.

| Résultat | Phase suivante | Erreur |
|----------|----------------|--------|
| Exactement 1 match, libre | Pulling | — |
| 0 match | Failed | `NoDeviceMatched` |
| > 1 match | Failed | `AmbiguousBinding` |
| 1 match, déjà occupé | Failed | `DeviceBusy` |

Un node est « occupé » si un autre McuDeployment (non Deployed/Failed) référence déjà `status.boundNode`.

### 6.2 Phase Pulling

Résout le manifeste OCI depuis `spec.firmware.image`.

1. Appelle `GET /v2/<repo>/manifests/<tag>` sur le registre.
2. Cherche la layer `application/vnd.embewi.firmware.bin` (fallback : première layer).
3. Stocke dans Status : `digest`, `size`, `deploymentId = string(dep.UID)`.
4. Passe en **Preparing**.

En cas d'erreur registre → requeue dans **30 s**.

### 6.3 Phase Preparing

1. Lit `GET /v1alpha1/info` pour idempotence §6 :

| `staged.state` | `staged.deploymentId` | Décision |
|----------------|-----------------------|----------|
| `activating` | == current | → skip → **Confirming** |
| `written` | == current et digest match | → skip → **Activating** |
| autre | — | → envoyer prepare |

2. Envoie `POST /v1alpha1/ota/prepare` :

```json
{
  "deployment_id": "<dep.UID>",
  "digest":        "sha256:...",
  "size":          983040,
  "chip":          "esp32c3",
  "idf_version":   "v6.0.0",
  "partition_layout": "embewi-ab-v1"
}
```

3. Si `accepted: false` → **Failed** avec le `reason` de l'agent.
4. Si `accepted: true` → **Writing**.

En cas d'erreur réseau → requeue dans **15 s**.

### 6.4 Phase Writing

1. Ouvre un stream blob depuis le registre OCI (`GET /v2/<repo>/blobs/<digest>`).
2. Pipe le stream directement vers `PUT /v1alpha1/ota/write` avec les headers :

```
Content-Type: application/octet-stream
Content-Length: <size>
X-Embewi-Deployment-Id: <deploymentId>
X-Embewi-Digest: sha256:...
Authorization: Bearer <token>
```

3. Vérifie que l'agent répond `{ "status": "written" }`.
4. Passe en **Activating**.

En cas d'erreur → requeue dans **30 s** (le slot n'est pas corrompu — la prochaine Preparing détectera `staged.state`).

### 6.5 Phase Activating

Envoie `POST /v1alpha1/ota/activate` :

```json
{ "deployment_id": "<deploymentId>", "reboot": true }
```

L'agent répond avant de redémarrer. Passe en **Confirming**.

En cas d'erreur → requeue dans **15 s**.

### 6.6 Phase Confirming

Attend la confirmation via heartbeat. Réévalué toutes les **10 s**.

Conditions de passage en **Deployed** :
```
node.status.state       == "running"
node.status.otaValidated == true
node.status.deploymentId == dep.status.deploymentId
```

Conditions de passage en **Failed** :
- `node.status.state ∈ { "failed", "rollback" }` → `DeviceRollback`
- Annotation `embewi.io/confirming-since` dépasse **2 minutes** → `ConfirmTimeout`

**Timeout négatif horodaté :**  
Au premier passage en Confirming, l'annotation `embewi.io/confirming-since` est posée avec le timestamp RFC3339 courant. Cette annotation persiste entre les cycles de réconciliation — le timer ne repart pas à zéro si le Core redémarre.

### 6.7 Phase Failed / Deployed

Terminales — plus de réconciliation. Pour relancer un déploiement échoué, créer un nouvel objet McuDeployment.

---

## 7. Client OCI

Package `internal/oci`. Utilise l'OCI Distribution Spec HTTP directement, sans bibliothèque externe.

### Résolution de manifeste

```
GET https://<registry>/v2/<repo>/manifests/<tag>
Accept: application/vnd.oci.image.manifest.v1+json,
        application/vnd.oci.artifact.manifest.v1+json,
        application/vnd.docker.distribution.manifest.v2+json
```

Extrait la layer `application/vnd.embewi.firmware.bin`. Si absente, prend la première layer (fallback permissif pour push simplifié).

Annotations lues (sur la layer, ou sur le manifeste en fallback) :
- `embewi.io/chip` — ex: `esp32c3`
- `embewi.io/idf-version` — ex: `v6.0.0`

### Stream de blob

```
GET https://<registry>/v2/<repo>/blobs/sha256:<hex>
```

Retourne un `io.ReadCloser` — le blob est streamé directement vers l'ESP sans buffer en mémoire.

### Schéma attendu du manifeste OCI

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": { "mediaType": "application/vnd.embewi.firmware.config.v1+json", ... },
  "layers": [
    {
      "mediaType": "application/vnd.embewi.firmware.bin",
      "digest": "sha256:b9e4f2...",
      "size": 983040,
      "annotations": {
        "embewi.io/chip": "esp32c3",
        "embewi.io/idf-version": "v6.0.0"
      }
    }
  ]
}
```

### Protocole HTTP vs HTTPS

| Registre | Protocole |
|----------|-----------|
| `localhost` ou `127.x.x.x` | HTTP automatique |
| Tout autre hôte | HTTPS |

Pour désactiver la vérification TLS (registre local auto-signé) : `OCI_INSECURE_TLS=true`.

---

## 8. Tokens d'authentification

Chaque device a un token Bearer unique généré au portail captif ESP.  
Le Core le charge depuis un **Secret Kubernetes**, jamais depuis des variables d'environnement.

### Structure du Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: embewi-tokens
  namespace: default
type: Opaque
data:
  esp32-motor-left:  YTNmN2MxYjIuLi4=   # base64(token hex)
  esp32-motor-right: YjllNGYyYzEuLi4=
```

La **clé** est le `spec.nodeId` du McuNode. La valeur est le token hex affiché au portail captif.

### Création

```bash
kubectl create secret generic embewi-tokens \
  --from-literal=esp32-motor-left="a3f7c1b2e8d09441f6bc3e7a2c504d8f" \
  --from-literal=esp32-motor-right="b9e4f2c1..."
```

### Lecture par le reconciler

À chaque `nodeClient()`, le reconciler fait un `GET` du Secret en live (pas de cache).  
Si la clé est absente : la phase échoue avec un message explicite — le McuDeployment reste dans sa phase courante (pas de Failed direct, requeue après timeout).

> **Namespace** : le Secret doit être dans le même namespace que le McuDeployment.

### Ajouter un device post-déploiement

```bash
kubectl patch secret embewi-tokens \
  --type=json \
  -p='[{"op":"add","path":"/data/esp32-new-node","value":"'$(echo -n "montoken" | base64)'"}]'
```

---

## 9. Configuration

### Flags de démarrage

| Flag | Défaut | Description |
|------|--------|-------------|
| `--heartbeat-address` | `:8080` | Adresse d'écoute du serveur heartbeat ESP→Core |
| `--metrics-bind-address` | `:8082` | Métriques Prometheus |
| `--health-probe-address` | `:8083` | Health probes (`/healthz`, `/readyz`) |
| `--token-secret` | `embewi-tokens` | Nom du Secret K8s contenant les tokens Bearer |
| `--leader-elect` | `false` | Activer l'élection de leader (multi-répliques) |

### Variables d'environnement

| Variable | Description |
|----------|-------------|
| `OCI_REGISTRY_USER` | Identifiant Basic auth pour le registre OCI |
| `OCI_REGISTRY_PASS` | Mot de passe Basic auth |
| `OCI_INSECURE_TLS` | `true` → skip vérification TLS (registre auto-signé) |
| `KUBECONFIG` | Chemin kubeconfig (hors cluster) — défaut `~/.kube/config` |

### Constantes internes

| Constante | Valeur | Description |
|-----------|--------|-------------|
| `HeartbeatTimeout` | 30 s | Délai sans heartbeat → `ready=false` |
| `ConfirmTimeout` | 2 min | Délai max pour confirmation après activate |
| Requeue Pulling error | 30 s | Retry si registre OCI injoignable |
| Requeue Preparing/Activating error | 15 s | Retry si agent ESP injoignable |
| Requeue Writing error | 30 s | Retry si stream OCI ou write ESP échoue |
| Requeue Confirming | 10 s | Polling heartbeat en phase Confirming |

---

## 10. Opérations

### Déclarer un nouveau device

```bash
cat <<EOF | kubectl apply -f -
apiVersion: embewi.io/v1alpha1
kind: McuNode
metadata:
  name: esp32-motor-left
  namespace: default
  labels:
    role: motor
spec:
  nodeId: esp32-motor-left
EOF
```

Ajouter son token au Secret :

```bash
kubectl patch secret embewi-tokens --type=json \
  -p='[{"op":"add","path":"/data/esp32-motor-left","value":"'$(echo -n "<token>" | base64)'"}]'
```

### Suivre l'état d'un device

```bash
kubectl get mcu esp32-motor-left -o wide
kubectl describe mcu esp32-motor-left   # conditions + lastHeartbeat
```

### Déployer un firmware

```bash
cat <<EOF | kubectl apply -f -
apiVersion: embewi.io/v1alpha1
kind: McuDeployment
metadata:
  name: wheel-controller-v1-1-0
  namespace: default
spec:
  nodeName: esp32-motor-left
  firmware:
    image: registry.local/embewi/wheel-controller:v1.1.0
    name: wheel-controller
    version: "1.1.0"
EOF
```

Suivre la progression en temps réel :

```bash
kubectl get mcudep wheel-controller-v1-1-0 -w
```

### Diagnostiquer un déploiement Failed

```bash
kubectl describe mcudep wheel-controller-v1-1-0
# Status:
#   Phase:    Failed
#   Message:  [ConfirmTimeout] aucune confirmation dans les 2m0s après activate
```

Causes possibles et actions :

| Message | Cause probable | Action |
|---------|----------------|--------|
| `NoDeviceMatched` | `spec.nodeName` incorrect | Vérifier `kubectl get mcu` |
| `DeviceBusy` | Autre McuDeployment actif sur ce node | Attendre ou supprimer l'autre |
| `prepare refusé: chip_mismatch` | Firmware compilé pour un autre chip | Rebuilder pour `esp32c3` |
| `stream blob: GET blob → HTTP 404` | Digest invalide ou image absente | Vérifier le registre OCI |
| `clé "esp32-motor-left" absente du secret` | Token manquant | Ajouter la clé dans `embewi-tokens` |
| `ConfirmTimeout` | Device n'a pas rebooté ou rollback silencieux | Consulter les logs `/v1alpha1/logs` |
| `DeviceRollback` | Self-check KO sur le device | Consulter les logs + heartbeat state |

### Recommencer un déploiement Failed

Les phases `Failed` et `Deployed` sont terminales. Pour réessayer :

```bash
kubectl delete mcudep wheel-controller-v1-1-0
kubectl apply -f wheel-controller-v1-1-0.yaml
```

### Vérifier le routage réseau

```bash
kubectl get endpointslices | grep embewi
kubectl describe endpointslice embewi-esp32-motor-left
# Endpoints:
#   Addresses: 192.168.10.50
#   Conditions: Ready=true
```

Accéder au service applicatif ESP depuis le cluster :

```bash
kubectl run -it --rm test --image=curlimages/curl -- \
  curl http://embewi-esp32-motor-left:8080/status
```

### Consulter les logs du controller

```bash
# Logs temps réel
kubectl logs -f deployment/embewi-core

# Filtrer par device
kubectl logs deployment/embewi-core | grep "esp32-motor-left"
```

Les logs des agents ESP remontent aussi dans les logs du controller via `/v1alpha1/logs`.

---

## Annexe — Ressources créées automatiquement

Pour chaque McuNode avec une IP connue, le controller crée dans le même namespace :

| Ressource | Nom | Contenu |
|-----------|-----|---------|
| `Service` | `embewi-<node-name>` | Port `status.appPort` (défaut 8080), selectorless |
| `EndpointSlice` | `embewi-<node-name>` | IP ESP, `ready=status.ready` |

Ces ressources sont réconciliées à chaque heartbeat. Supprimer le McuNode ne les supprime pas automatiquement en MVP.
