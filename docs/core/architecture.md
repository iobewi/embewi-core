# Architecture

## Vue d'ensemble

```text
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

- **Inbound** (Core → ESP) : requêtes HTTPS initiées par le Core vers l'agent. Le
  Core orchestre les phases OTA, pousse la config, lit l'état.
- **Outbound** (ESP → Core) : heartbeats et logs initiés par l'agent vers le
  heartbeat server. Le Core ne poll jamais l'agent pour connaître son état — il
  attend les heartbeats.

## Composants

| Package | Rôle |
|---------|------|
| `internal/controller/mcunode_controller.go` | Réconcilie McuNode : Service + EndpointSlice, pilotage `ready` |
| `internal/controller/mcudeployment_controller.go` | Machine d'état OTA : Binding → Pulling → … → Deployed |
| `internal/heartbeat/server.go` | Reçoit heartbeats et logs ESP, patche McuNode.Status |
| `internal/oci/client.go` | Résolution manifeste OCI + stream blob sans buffer |
| `internal/agent/client.go` | Client HTTPS vers l'agent (prepare, write, activate, config) |
| `api/v1alpha1/` | Types CRD + DeepCopy générés |

## Heartbeat server

Serveur HTTP simple (flux interne cluster, pas HTTPS) qui reçoit les flux sortants ESP.
Adresse par défaut : `:8080`. Configuré via `--heartbeat-address`.

### `POST /v1alpha1/heartbeat`

Met à jour le `McuNode.Status` correspondant au `node_id` reçu.

**Corps attendu :**

```json
{
  "node_id":         "esp32-motor-left",
  "ts":              1710000000,
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
2. Extrait l'IP source depuis `RemoteAddr` — le champ `ip` du payload n'est pas
   utilisé comme source de vérité réseau (§5 contrat).
3. Patch `McuNode.Status` : ip, state, firmwareDigest, deploymentId, otaValidated,
   heapFree, rssi, uptimeMs, ready, lastHeartbeat.
4. Répond toujours **200** — même si le McuNode est inconnu, pour ne pas crasher
   l'agent sur un 404.

**`ready` calculé localement :**

```text
ready = (state == "running") && ota_validated
```

Le McuNode reconciler affine ensuite avec le timeout heartbeat (30 s).

### `POST /v1alpha1/logs`

Reçoit les logs d'événements de l'agent. Pas de persistance — écriture dans le
logger zap du controller.

**Corps attendu :**

```json
{
  "ts":       1710000000,
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
