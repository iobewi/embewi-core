# Contrôleurs

## McuNode reconciler

Déclenché à chaque modification d'un McuNode (`internal/controller/mcunode_controller.go`).

### Pilotage de `ready`

```text
wantReady = state=="running" && otaValidated && time.Since(lastHeartbeat) ≤ 10s
```

Si `wantReady` ou `state` change → patch status. La condition est réévaluée toutes
les **10 s** via `RequeueAfter` (= `HeartbeatTimeout`).

Cas de passage à `ready=false` :
- Heartbeat silencieux depuis > 10 s → `state` basculé en `offline`
- `state` ≠ `running` (pending_verify, degraded, rollback, failed)
- `otaValidated == false`

### Service selectorless

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
      port: <status.appPort>    # défaut 8080, peuplé depuis GET /info
      protocol: TCP
  # Pas de selector — l'EndpointSlice est géré manuellement
```

### EndpointSlice

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
  - addresses: ["192.168.10.50"]    # status.ip — source : heartbeat.ip, jamais RemoteAddr
    conditions:
      ready: true                   # piloté par status.ready
ports:
  - name: app
    port: 8080
    protocol: TCP
```

`ready` suit `McuNode.Status.Ready` — jamais statique. C'est le seul signal
réseau que le cluster utilise pour savoir si l'ESP est opérationnel.

Le McuNode est défini comme **owner** du Service et de l'EndpointSlice via
`controllerutil.SetControllerReference` — suppression en cascade automatique.

---

## McuDeployment reconciler

Machine d'état pilotée par `Status.Phase`
(`internal/controller/mcudeployment_controller.go`). Chaque appel à `Reconcile`
exécute exactement une phase, puis requeue.

### Phase Binding

Résout le McuNode cible selon `spec.nodeName` (prioritaire) ou `spec.nodeSelector`.

| Résultat | Phase suivante | Erreur |
|----------|----------------|--------|
| Exactement 1 match, libre | Pulling | — |
| 0 match | Failed | `NoDeviceMatched` |
| > 1 match | Failed | `AmbiguousBinding` |
| 1 match, déjà occupé | Failed | `DeviceBusy` |

Un node est « occupé » si un autre McuDeployment (non Deployed/Failed) référence
déjà `status.boundNode`.

### Phase Pulling

Résout le manifeste OCI depuis `spec.firmware.image`.

1. Appelle `GET /v2/<repo>/manifests/<tag>` sur le registre.
2. Cherche la layer `application/vnd.embewi.firmware.bin` (fallback : première layer).
3. Stocke dans Status : `digest`, `size`, `deploymentId = string(dep.UID)`.
4. Passe en **Preparing**.

En cas d'erreur registre → requeue dans **30 s**.

### Phase Preparing

1. Si `spec.configMapRef` renseigné : pousse la config **avant** l'OTA (§6 contrat).
   En cas d'erreur permanente (clé > 15 chars, CM introuvable) → `Failed/ConfigInvalid`.

2. Lit `GET /v1alpha1/info` pour idempotence (§6 contrat). Persiste aussi
   `chip`, `idfVersion`, `flashSize`, `ramSize`, `appPort` dans `McuNode.Status` :

| `staged.state` | `staged.deploymentId` | Décision |
|----------------|-----------------------|----------|
| `activating` | == current | → skip → **Confirming** |
| `written` + digest match | == current | → skip → **Activating** |
| autre | — | → envoyer prepare |

3. Envoie `POST /v1alpha1/ota/prepare` :

```json
{
  "deployment_id":    "<dep.UID>",
  "artifact":         "registry.local/embewi/wheel-controller:v1.1.0",
  "digest":           "sha256:...",
  "size":             983040,
  "chip":             "esp32c3",
  "idf_version":      "v6.0.0",
  "partition_layout": "embewi-ab-v1"
}
```

4. Si `accepted: false` → **Failed** + Event K8s (voir tableau §4b).
5. Si `accepted: true` → **Writing**.

En cas d'erreur réseau → requeue dans **15 s**.

### Phase Writing

1. Ouvre un stream blob depuis le registre OCI (`GET /v2/<repo>/blobs/<digest>`).
2. Pipe le stream vers `PUT /v1alpha1/ota/write` avec les headers :

```
Content-Type:            application/octet-stream
Content-Length:          <size>
Content-Range:           bytes 0-<size-1>/<size>
X-Embewi-Deployment-Id: <deploymentId>
X-Embewi-Digest:         sha256:...
Authorization:           Bearer <token>
```

3. Vérifie que l'agent répond `{ "status": "written" }`.
4. Passe en **Activating**.

`Content-Range: bytes 0-{n-1}/{n}` signale une nouvelle session (§4 contrat).
La reprise intra-session (start > 0) est supportée par l'agent mais pas encore
implémentée côté Core (le retry repart de l'octet 0).

En cas d'erreur → requeue dans **30 s** (le slot inactif n'est pas corrompu —
la prochaine Preparing détectera `staged.state`).

### Phase Activating

Vérifie d'abord l'idempotence via `GET /v1alpha1/info` :

- Si `staged.state == "activating"` ET bon `deploymentId` → activate déjà envoyé → skip directement **Confirming**
- Si `state == "pending_verify"` → device en cours de self-check → skip directement **Confirming**
- Sinon → envoie `POST /v1alpha1/ota/activate` :

```json
{ "deployment_id": "<deploymentId>", "reboot": true }
```

L'agent répond avant de redémarrer. Passe en **Confirming**.

En cas d'erreur → requeue dans **15 s**.

### Phase Confirming

Attend la confirmation via heartbeat. Réévalué toutes les **10 s**.
Déclenché aussi immédiatement à chaque mise à jour du McuNode (watch).

Conditions de passage en **Deployed** :

```text
node.status.state        == "running"
node.status.otaValidated == true
node.status.deploymentId == dep.status.deploymentId
```

Conditions de passage en **Failed** :

- `node.status.state ∈ { "failed", "rollback", "degraded" }` → `DeviceRollback`
- Annotation `embewi.io/confirming-since` dépasse **2 minutes** → `ConfirmTimeout`

**Timeout négatif horodaté :** au premier passage en Confirming, l'annotation
`embewi.io/confirming-since` est posée avec le timestamp RFC3339 courant. Cette
annotation persiste entre les cycles de réconciliation — le timer ne repart pas
à zéro si le Core redémarre.

### Phase Deployed

La réconciliation **n'est pas terminale** en phase Deployed. Le reconciler surveille :

1. **`Available/HeartbeatTimeout`** : si le McuNode perd ses heartbeats (> 10 s),
   `Available` passe à `False/HeartbeatTimeout`. Reprend à `WorkloadReady` dès
   que les heartbeats reprennent. Déclenché par le watch McuNode.

2. **Config drift** (si `spec.configMapRef` renseigné) : si le McuConfigMap change,
   la config est re-poussée et le device reboot. Déclenché par le watch McuConfigMap.

### Phase Failed

Terminale. Pour relancer un déploiement échoué, créer un nouvel objet McuDeployment.

### Condition Ready (synthétique)

```text
Ready=True  ← Progressing=False ET Available=True
Ready=False ← dans tous les autres cas
```

```bash
# Attendre la fin d'un déploiement :
kubectl wait mcudeployment/wheel-left --for=condition=Ready --timeout=5m
```

---

## Serveur heartbeat

(`internal/heartbeat/server.go`) — écoute les flux sortants ESP → Core.

### `POST /v1alpha1/heartbeat`

Reçu toutes les 5 s par device. Met à jour `McuNode.Status` : IP, state,
otaValidated, métriques temps réel, conditions Provisioned/Ready.

Authentification : Bearer token validé par comparaison temps-constant contre le
Secret référencé dans `McuNodeSpec.TokenRef`.

### `wss://.../v1alpha1/logs` (WebSocket)

L'agent ouvre une connexion WS **cliente** (outbound) vers le Core. Le Core
est serveur.

- Auth différée sur le premier frame : `"node"` identifie le device, le Bearer
  header est validé contre `spec.tokenRef`.
- Si token invalide : close frame `1008 ClosePolicyViolation`.
- Best-effort : pas de replay sur reconnexion.

### `POST /v1alpha1/logs`

Fallback HTTP pour les événements OTA/lifecycle critiques. Même path que le WS —
le serveur détecte l'upgrade automatiquement.
