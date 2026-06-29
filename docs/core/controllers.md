# Contrôleurs

## McuNode reconciler

Déclenché à chaque modification d'un McuNode (`internal/controller/mcunode_controller.go`).

### Pilotage de `ready`

```text
wantReady = state=="running" && otaValidated && time.Since(lastHeartbeat) ≤ 30s
```

Si `wantReady != status.ready` → patch status. La condition est réévaluée toutes
les **30 s** via `RequeueAfter`.

Cas de passage à `ready=false` :
- Heartbeat silencieux depuis > 30 s
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
      port: <status.appPort>    # défaut 8080
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
  - addresses: ["192.168.10.50"]    # status.ip
    conditions:
      ready: true                   # piloté par status.ready
ports:
  - name: app
    port: 8080
    protocol: TCP
```

`ready` suit `McuNode.Status.Ready` — jamais statique. C'est le seul signal
réseau que le cluster utilise pour savoir si l'ESP est opérationnel.

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

1. Lit `GET /v1alpha1/info` pour idempotence (§6 contrat) :

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

### Phase Writing

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

En cas d'erreur → requeue dans **30 s** (le slot n'est pas corrompu — la prochaine
Preparing détectera `staged.state`).

### Phase Activating

Envoie `POST /v1alpha1/ota/activate` :

```json
{ "deployment_id": "<deploymentId>", "reboot": true }
```

L'agent répond avant de redémarrer. Passe en **Confirming**.

En cas d'erreur → requeue dans **15 s**.

### Phase Confirming

Attend la confirmation via heartbeat. Réévalué toutes les **10 s**.

Conditions de passage en **Deployed** :

```text
node.status.state        == "running"
node.status.otaValidated == true
node.status.deploymentId == dep.status.deploymentId
```

Conditions de passage en **Failed** :

- `node.status.state ∈ { "failed", "rollback" }` → `DeviceRollback`
- Annotation `embewi.io/confirming-since` dépasse **2 minutes** → `ConfirmTimeout`

**Timeout négatif horodaté :** au premier passage en Confirming, l'annotation
`embewi.io/confirming-since` est posée avec le timestamp RFC3339 courant. Cette
annotation persiste entre les cycles de réconciliation — le timer ne repart pas
à zéro si le Core redémarre.

### Phase Failed / Deployed

Terminales — plus de réconciliation. Pour relancer un déploiement échoué, créer
un nouvel objet McuDeployment.
