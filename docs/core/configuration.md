# Configuration

## Flags de démarrage

| Flag | Défaut | Description |
|------|--------|-------------|
| `--heartbeat-address` | `:8080` | Adresse d'écoute du serveur heartbeat/logs ESP→Core |
| `--heartbeat-tls-cert` | `""` | Chemin PEM du certificat TLS (vide = HTTP plain) |
| `--heartbeat-tls-key` | `""` | Chemin PEM de la clé privée TLS |
| `--metrics-bind-address` | `:8082` | Métriques Prometheus |
| `--health-probe-address` | `:8083` | Health probes (`/healthz`, `/readyz`) |
| `--token-secret` | `embewi-tokens` | Fallback Secret centralisé si `spec.tokenRef` absent |
| `--leader-elect` | `false` | Activer l'élection de leader (multi-répliques) |

> En production, les devices ESP imposent HTTPS (§1 contrat) : configurer
> `--heartbeat-tls-cert` et `--heartbeat-tls-key`, ou terminer TLS à l'ingress.

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
| `HeartbeatTimeout` | **10 s** | Délai sans heartbeat → `ready=false` (2 × période agent 5 s, §8a contrat) |
| `ConfirmTimeout` | 2 min | Délai max pour confirmation après activate (§3 contrat) |
| Requeue Pulling error | 30 s | Retry si registre OCI injoignable |
| Requeue Preparing/Activating error | 15 s | Retry si agent ESP injoignable |
| Requeue Writing error | 30 s | Retry si stream OCI ou write ESP échoue |
| Requeue Confirming | 10 s | Polling heartbeat en phase Confirming |

## Token Bearer — Secret par device

Chaque device a un token Bearer unique (§1 contrat). Le Core le charge depuis un
Secret Kubernetes référencé dans `McuNodeSpec.TokenRef` — **un Secret par device**.

```yaml
# Secret dédié au device esp32-motor-left
apiVersion: v1
kind: Secret
metadata:
  name: esp32-motor-left-token
  namespace: default
type: Opaque
stringData:
  token: "a3f7c1b2e8d09441f6bc3e7a2c504d8f"
```

```yaml
# McuNode qui référence ce Secret
spec:
  nodeId: esp32-motor-left
  tokenRef:
    name: esp32-motor-left-token
    namespace: default          # optionnel — défaut = namespace du McuNode
```

Le token est rechargé à chaque appel `nodeClient()` — pas de cache en mémoire.

### Fallback Secret centralisé (migration)

Si `spec.tokenRef.name` est vide (McuNodes créés avant la v0.x), le Core
utilise le Secret centralisé `--token-secret` (défaut `embewi-tokens`) avec
`nodeId` comme clé :

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: embewi-tokens
  namespace: default
type: Opaque
data:
  esp32-motor-left:  YTNmN2MxYjIuLi4=   # base64(token)
  esp32-motor-right: YjllNGYyYzEuLi4=
```

> Migrer vers `spec.tokenRef` est recommandé — le Secret centralisé est
> maintenu pour compatibilité ascendante uniquement.
