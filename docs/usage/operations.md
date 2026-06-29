# Opérations courantes

## Vérifier le routage réseau

Le Core crée automatiquement un `Service` + `EndpointSlice` pour chaque McuNode
avec une IP. Le flag `ready` de l'EndpointSlice suit l'état du device en temps réel.

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

Le port est celui retourné par `GET /info` (`app_port`, défaut 8080). Il est
stocké dans `McuNode.Status.AppPort`.

---

## Consulter les logs

```bash
# Logs temps réel du controller
kubectl logs -f deployment/embewi-core

# Filtrer par device
kubectl logs deployment/embewi-core | grep "esp32-motor-left"
```

Les logs des agents ESP (events OTA, self-check, boot) remontent dans les logs
du controller via `POST /v1alpha1/logs`.

---

## Métriques Prometheus

Le Core expose `/metrics` sur `:8082` (flag `--metrics-bind-address`).
Chaque heartbeat met à jour les gauges du device via le label `node_id`.

| Métrique | Type | Source |
|----------|------|--------|
| `mcunode_heap_free_bytes` | gauge | `heap_free` |
| `mcunode_wifi_rssi_dbm` | gauge | `rssi` |
| `mcunode_uptime_seconds` | gauge | `uptime_ms / 1000` |
| `mcunode_temperature_celsius` | gauge | `temp_celsius` (filtrée si `-127.0`) |
| `mcunode_task_stack_hwm_bytes` | gauge | `task_hwm_min` |
| `mcunode_last_heartbeat_timestamp` | gauge | `ts` |
| `mcunode_config_generation` | gauge | `config_generation` |
| `mcunode_ota_validated` | gauge | `ota_validated` → 0/1 |

Labels communs sur toutes les métriques : `node_id`, `workload`, `chip`.

Pour scraper avec Prometheus Operator :

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: embewi-core
  namespace: embewi
spec:
  selector:
    matchLabels:
      app: embewi-core
  endpoints:
    - port: metrics
      path: /metrics
      interval: 30s
```

---

## Rotation de token

```bash
# 1. Générer un nouveau token
NEW_TOKEN=$(openssl rand -hex 16)

# 2. Mettre à jour le Secret K8s
kubectl patch secret embewi-tokens \
  --type=json \
  -p='[{"op":"replace","path":"/data/esp32-motor-left","value":"'$(echo -n "$NEW_TOKEN" | base64)'"}]'

# 3. Appeler POST /token sur le device (avec l'ancien token encore valide)
OLD_TOKEN=$(kubectl get secret embewi-tokens \
  -o jsonpath='{.data.esp32-motor-left}' | base64 -d)
curl -k -X POST https://192.168.10.50/v1alpha1/token \
  -H "Authorization: Bearer $OLD_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"token\":\"$NEW_TOKEN\"}"
# → {"status":"ok"}
```

Dès la réponse 200, seul `newToken` est valide — le device a committé en NVS.
Le Core utilise automatiquement le nouveau token dès le prochain appel (lecture
live du Secret).
