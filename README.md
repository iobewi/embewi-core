# Embewi Core

Kubernetes Runtime Controller pour devices ESP32 (MCU).  
Gère le cycle de vie OTA, l'état des devices et le routage réseau via EndpointSlices.

## Architecture

```
ESP32 Agent (HTTPS :443)          Embewi Core (Go)
┌─────────────────────┐          ┌──────────────────────────────┐
│ GET  /v1alpha1/info │◄─────────┤ McuDeploymentReconciler      │
│ POST /ota/prepare   │◄─────────┤  Binding → Pulling →         │
│ PUT  /ota/write     │◄─────────┤  Preparing → Writing →       │
│ POST /ota/activate  │◄─────────┤  Activating → Confirming →   │
│                     │          │  Deployed / Failed            │
│ POST heartbeat ─────┼─────────►│ heartbeat.Server             │
│ POST logs      ─────┼─────────►│  → patch McuNode.Status      │
└─────────────────────┘          │                              │
                                 │ McuNodeReconciler            │
                                 │  → Service + EndpointSlice   │
                                 └──────────────────────────────┘
                                          │
                                 ┌────────▼────────┐
                                 │  k0s / k3s      │
                                 │  Kubernetes API  │
                                 └─────────────────┘
```

## Prérequis

- Go 1.22+
- kubectl configuré sur le cluster k0s
- Cluster k0s/k3s avec accès admin

## Installation rapide

### 1. Récupérer le kubeconfig k0s

```bash
# Sur le node k0s :
sudo k0s kubeconfig admin > ~/.kube/config

# Vérifier :
kubectl cluster-info
kubectl get nodes
```

### 2. Installer kubectl (si absent)

```bash
curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
chmod +x kubectl && sudo mv kubectl /usr/local/bin/
```

### 3. Installer les CRDs

```bash
kubectl apply -f config/crd/bases/
```

Vérification :
```bash
kubectl get crds | grep embewi.io
# embewi.io_mcunodes.yaml
# embewi.io_mcudeployments.yaml

kubectl get mcu        # alias McuNode
kubectl get mcudep     # alias McuDeployment
```

### 4. Builder le binaire

```bash
go build -o bin/embewi-core ./cmd/controller/
```

### 5. Créer le Secret des tokens

```bash
# Token récupéré depuis la page de confirmation du portail captif
kubectl create secret generic embewi-tokens \
  --from-literal=esp32-motor-left="<token affiché au portail>" \
  --from-literal=esp32-motor-right="<token>"
```

### 6. Lancer le controller (hors cluster, mode dev)

```bash
# Registre OCI avec auth Basic (optionnel) :
export OCI_REGISTRY_USER=myuser
export OCI_REGISTRY_PASS=mypass
# Désactiver la vérif TLS pour registre local auto-signé (optionnel) :
export OCI_INSECURE_TLS=true

./bin/embewi-core \
  --heartbeat-address=:8080 \
  --metrics-bind-address=:8082 \
  --health-probe-address=:8083 \
  --token-secret=embewi-tokens
```

> Le controller utilise `~/.kube/config` automatiquement hors cluster.

### 7. Déploiement in-cluster (production)

```bash
# 1. Builder et pousser l'image sur le registre
make docker-build docker-push IMG=registry.local/embewi/core:latest

# Sur k0s sans registre : importer l'image directement
docker save embewi/core:latest | ssh <node-k0s> sudo k0s ctr images import -

# 2. Installer CRDs + RBAC + Deployment en une commande
make deploy IMG=registry.local/embewi/core:latest

# 3. Créer le Secret des tokens dans le bon namespace
kubectl create secret generic embewi-tokens \
  --namespace embewi-system \
  --from-literal=<nodeId>="<token>"

# 4. Vérifier
kubectl rollout status deployment/embewi-core -n embewi-system
kubectl logs -n embewi-system -l app=embewi-core -f
```

Le port heartbeat est exposé en **NodePort 30880** — c'est cette adresse que tu provisiones comme `ctrl_url` dans le portail captif :
```
http://<IP-node-k0s>:30880
```

---

## Utilisation

### Déclarer un device

```bash
kubectl apply -f config/samples/mcunode_sample.yaml
```

```yaml
apiVersion: embewi.io/v1alpha1
kind: McuNode
metadata:
  name: esp32-motor-left
  namespace: default
  labels:
    role: motor
spec:
  nodeId: esp32-motor-left   # doit correspondre à EMBEWI_NODE_ID dans le firmware
```

Dès que le device envoie son premier heartbeat, le status se peuple automatiquement :

```bash
kubectl get mcu esp32-motor-left -o wide
# NAME               NODE ID              IP              STATE     VERSION   READY
# esp32-motor-left   esp32-motor-left     192.168.10.50   running   1.0.0     true

kubectl describe mcu esp32-motor-left
# Status:
#   Ip:                192.168.10.50
#   State:             running
#   Firmware Version:  1.0.0
#   Firmware Digest:   sha256:a3f7c1...
#   Ota Validated:     true
#   Heap Free:         82344
#   Rssi:              -61
#   Ready:             true
#   Last Heartbeat:    2026-06-16T10:30:00Z
```

### Déployer un firmware

```bash
kubectl apply -f config/samples/mcudeployment_sample.yaml
```

Suivre la progression :

```bash
kubectl get mcudep -w
# NAME                       NODE                  IMAGE                                         PHASE
# wheel-controller-v1-1-0   esp32-motor-left       registry.local/embewi/wheel-controller:v1.1.0  Binding
# wheel-controller-v1-1-0   esp32-motor-left       ...                                            Preparing
# wheel-controller-v1-1-0   esp32-motor-left       ...                                            Writing
# wheel-controller-v1-1-0   esp32-motor-left       ...                                            Activating
# wheel-controller-v1-1-0   esp32-motor-left       ...                                            Confirming
# wheel-controller-v1-1-0   esp32-motor-left       ...                                            Deployed
```

---

## Ports

| Port | Rôle |
|------|------|
| `:8080` | Heartbeat server — reçoit POST ESP→Core (`/v1alpha1/heartbeat`, `/v1alpha1/logs`) |
| `:8082` | Métriques Prometheus |
| `:8083` | Health probes (`/healthz`, `/readyz`) |

> L'ESP doit pouvoir atteindre `:8080` — c'est le `ctrl_url` provisionné au portail captif.  
> Exemple : `http://192.168.10.1:8080`

---

## Tokens d'authentification

Chaque device a un token Bearer unique généré au portail captif et affiché **une seule fois**.  
Le controller les lit depuis un Secret Kubernetes — clé = `nodeId`, valeur = token hex.

```bash
kubectl create secret generic embewi-tokens \
  --from-literal=esp32-motor-left="a3f7c1b2e8d09441f6bc3e7a2c504d8f" \
  --from-literal=esp32-motor-right="b9e4f2c1..."
```

Le nom du Secret est configurable via `--token-secret` (défaut : `embewi-tokens`).

---

## Registre OCI

Les firmwares sont stockés comme artefacts OCI. Structure attendue du manifeste :

```json
{
  "schemaVersion": 2,
  "layers": [{
    "mediaType": "application/vnd.embewi.firmware.bin",
    "digest": "sha256:...",
    "size": 983040,
    "annotations": {
      "embewi.io/chip": "esp32c3",
      "embewi.io/idf-version": "v6.0.0"
    }
  }]
}
```

Variables d'environnement :

| Variable | Description |
|----------|-------------|
| `OCI_REGISTRY_USER` | Identifiant Basic auth (optionnel) |
| `OCI_REGISTRY_PASS` | Mot de passe Basic auth (optionnel) |
| `OCI_INSECURE_TLS`  | `true` → skip TLS verify (registre local auto-signé) |

Pour les registres `localhost` ou `127.x.x.x`, HTTP est utilisé automatiquement.

---

## Ressources Kubernetes créées automatiquement

Pour chaque McuNode dont le status contient une IP, le controller crée :

**Service** (selectorless) :
```
embewi-<node-name>  →  port app (défaut 8080)
```

**EndpointSlice** :
```
embewi-<node-name>  →  IP ESP, ready=<status.ready>
```

`ready` est `true` uniquement si `state==running && ota_validated==true && heartbeat récent (<30s)`.

---

## Structure du projet

```
embewi-core/
├── api/v1alpha1/
│   ├── mcunode_types.go              CRD McuNode
│   ├── mcudeployment_types.go        CRD McuDeployment
│   ├── groupversion_info.go          groupe embewi.io/v1alpha1
│   └── zz_generated.deepcopy.go     DeepCopy (bootstrap manuel)
├── internal/
│   ├── agent/client.go               Client HTTPS vers l'agent ESP
│   ├── oci/client.go                 Client OCI Distribution Spec (pull firmware)
│   ├── heartbeat/server.go           Serveur heartbeat ESP→Core
│   └── controller/
│       ├── mcunode_controller.go     EndpointSlice + timeout heartbeat
│       └── mcudeployment_controller.go  Machine d'état OTA
├── cmd/controller/main.go            Point d'entrée
├── config/
│   ├── crd/bases/                    CRDs YAML à appliquer sur le cluster
│   └── samples/                      Exemples McuNode + McuDeployment
└── Makefile
```

---

## Makefile

```bash
make build       # compile le binaire dans bin/
make tidy        # go mod tidy
make manifests   # régénère les CRDs YAML (nécessite controller-gen)
make generate    # régénère zz_generated.deepcopy.go (nécessite controller-gen)
```

### Installer controller-gen (optionnel, pour regénérer les CRDs)

```bash
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
```

---

## État MVP — ce qui est implémenté

| Composant | État |
|-----------|------|
| CRDs McuNode + McuDeployment | ✅ |
| Heartbeat server (POST ESP→Core) | ✅ |
| Patch McuNode.Status depuis heartbeat | ✅ |
| Service + EndpointSlice par McuNode | ✅ |
| Timeout heartbeat → ready=false | ✅ |
| Binding McuDeployment → McuNode | ✅ |
| Machine d'état OTA (phases) | ✅ |
| Client HTTPS agent ESP | ✅ |
| Pull OCI firmware (manifest + blob) | ✅ |
| Stream binaire PUT /ota/write | ✅ |
| Tokens depuis Secret K8s | ✅ |
| Déploiement in-cluster (Deployment + RBAC) | ✅ |
