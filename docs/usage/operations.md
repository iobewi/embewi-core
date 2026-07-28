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
  namespace: embewi-system
spec:
  selector:
    matchLabels:
      app: embewi-core
  endpoints:
    - port: metrics
      path: /metrics
      interval: 30s
```

Nécessite un Service exposant un port **nommé** `metrics` (ex.
`embewi-core-metrics`, `config/manager/deployment.yaml`) — sans ça, le
ServiceMonitor n'a aucun Endpoints à scraper malgré `/metrics` actif sur le
conteneur.

---

## Rotation de token

Nécessite `spec.tokenRef` sur le McuNode (Secret dédié par device — voir
`config/samples/mcunode_sample.yaml`). Le fallback historique (Secret
centralisé `embewi-tokens` keyé par `nodeId`, sans `tokenRef`) ne supporte
**pas** la rotation automatisée ci-dessous.

Le Core rejoue lui-même `POST /token` — pas d'appel `curl` manuel à faire :

```bash
# Écriture ATOMIQUE de token + previousToken (un seul kubectl patch) : sans ça,
# un crash Core juste après l'écriture du nouveau token rend le device
# injoignable en inbound (previousToken est ce qui permet au Core de rejouer
# POST /token tant que le device n'a pas confirmé — seul recours sinon : le
# portail captif de reprovisioning).
OLD_TOKEN=$(kubectl get secret esp32-motor-left-token \
  -o jsonpath='{.data.token}' | base64 -d)
NEW_TOKEN=$(openssl rand -hex 16)

kubectl patch secret esp32-motor-left-token --type=json -p="[
  {\"op\":\"replace\",\"path\":\"/data/token\",\"value\":\"$(echo -n "$NEW_TOKEN" | base64)\"},
  {\"op\":\"add\",\"path\":\"/data/previousToken\",\"value\":\"$(echo -n "$OLD_TOKEN" | base64)\"}
]"
```

À partir de là, entièrement automatique (`internal/controller/mcunode_controller.go`,
`reconcileTokenRotation` + `internal/heartbeat/server.go`, `clearPreviousToken`) :

```text
1. Reconcile McuNode détecte previousToken → POST /token (auth previousToken,
   corps newToken), rejoué à chaque reconcile tant que non confirmé.
2. Device commite newToken en NVS avant de répondre (atomique, §4 contrat).
3. Premier heartbeat authentifié avec newToken → previousToken effacé du
   Secret par le Core.
```

Suivre la progression :

```bash
kubectl get events --field-selector reason=TokenRotationApplied
kubectl get secret esp32-motor-left-token -o jsonpath='{.data.previousToken}'
# vide (absent) une fois la rotation confirmée
```

## Rebooter un device à la demande

Pas de `kubectl rollout restart` pour un CRD (spécifique aux
`deployment`/`daemonset`/`statefulset`) ni de `kubectl delete pod` équivalent
(`delete McuNode` ne touche jamais au device physique — le CRD le
représente, il ne le possède pas). À la place, une annotation :

```bash
kubectl annotate mcunode esp32-motor-left \
  embewi.io/reboot-requested="$(date +%s)" --overwrite
```

```bash
kubectl get events --field-selector reason=RebootRequested
kubectl get mcunode esp32-motor-left -o jsonpath='{.status.lastRebootRequested}'
```

Une nouvelle valeur (différente de `status.lastRebootRequested`) redéclenche
un reboot — pas besoin de retirer l'annotation entre deux usages.

## Décommissionner un device en sécurité (firmware « safe-mode »)

`delete McuNode` ne touche jamais au device physique (cf. ci-dessus) — un
actionneur (moteur, relais…) piloté par le workload en cours continue de
tourner sur son dernier état GPIO, orphelin de toute gestion Core. Pour
mettre un device hors service **en sécurité** avant de le débrancher/déplacer,
ne pas essayer de forcer un rollback (§3 contrat) : ce mécanisme n'est pas
pilotable par le Core (il ne s'auto-déclenche que côté device sur échec de
self-check en `pending_verify`) et revient sur l'**autre slot OTA**, quel
qu'il soit — pas une image choisie, potentiellement une ancienne version qui
pilote encore les mêmes GPIO.

**Approche recommandée** : un déploiement OTA normal vers un firmware
« safe-mode » dédié — même agent embewi actif (heartbeat, joignable,
gérable), mais qui ne touche **aucun** GPIO actionneur, inerte par
construction. Aucun mécanisme Core spécial : le pipeline OTA standard
(chunking, digest, signature, idempotence, confirmation par heartbeat)
s'applique tel quel.

```bash
# 1. Construire/publier le firmware safe-mode une fois (registre OCI),
#    par ex. registry.local/embewi/safe-mode:v1.0.0

# 2. Basculer le device dessus comme n'importe quel autre déploiement
kubectl patch mcudeployment wheel-left --type=merge -p '
spec:
  firmware:
    image: registry.local/embewi/safe-mode:v1.0.0
'

# 3. Attendre la confirmation avant de débrancher/déplacer physiquement
kubectl wait mcudeployment/wheel-left --for=condition=Available --timeout=5m
kubectl get mcunode esp32-motor-left -o jsonpath='{.status.state} {.status.otaValidated}'
# running true → GPIO inertes, device peut être manipulé sans risque
```

**Limite** : ne fonctionne que si le device est encore **joignable** (Wi-Fi
up, agent qui répond) — adapté à un décommissionnement planifié, pas à un
device déjà bloqué/injoignable (aucun moyen de pousser un OTA vers un
device qu'on ne peut pas contacter).

Une fois le device physiquement retiré, supprimer le `McuDeployment` puis le
`McuNode` (bookkeeping administratif seul, cf. ci-dessus).
