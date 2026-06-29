# Configuration

## Flags de démarrage

| Flag | Défaut | Description |
|------|--------|-------------|
| `--heartbeat-address` | `:8080` | Adresse d'écoute du serveur heartbeat ESP→Core |
| `--metrics-bind-address` | `:8082` | Métriques Prometheus |
| `--health-probe-address` | `:8083` | Health probes (`/healthz`, `/readyz`) |
| `--token-secret` | `embewi-tokens` | Nom du Secret K8s contenant les tokens Bearer |
| `--leader-elect` | `false` | Activer l'élection de leader (multi-répliques) |

## Variables d'environnement

| Variable | Description |
|----------|-------------|
| `OCI_REGISTRY_USER` | Identifiant Basic auth pour le registre OCI |
| `OCI_REGISTRY_PASS` | Mot de passe Basic auth |
| `OCI_INSECURE_TLS` | `true` → skip vérification TLS (registre auto-signé) |
| `KUBECONFIG` | Chemin kubeconfig (hors cluster) — défaut `~/.kube/config` |

## Constantes internes

| Constante | Valeur | Description |
|-----------|--------|-------------|
| `HeartbeatTimeout` | 30 s | Délai sans heartbeat → `ready=false` |
| `ConfirmTimeout` | 2 min | Délai max pour confirmation après activate |
| Requeue Pulling error | 30 s | Retry si registre OCI injoignable |
| Requeue Preparing/Activating error | 15 s | Retry si agent ESP injoignable |
| Requeue Writing error | 30 s | Retry si stream OCI ou write ESP échoue |
| Requeue Confirming | 10 s | Polling heartbeat en phase Confirming |

## Secret des tokens

Chaque device a un token Bearer unique généré au portail captif ESP. Le Core le
charge depuis un **Secret Kubernetes** à chaque `nodeClient()` (pas de cache).

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

La **clé** est le `spec.nodeId` du McuNode. La valeur est le token hex affiché
au portail captif, encodé en base64.

> Le Secret doit être dans le **même namespace** que le McuDeployment.
