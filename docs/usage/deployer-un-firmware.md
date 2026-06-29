# Déployer un firmware

Un `McuDeployment` orchestre le transfert OTA d'un firmware depuis un registre
OCI vers un device ESP32. Le Core gère toutes les phases automatiquement.

---

## 1. Préparer l'image OCI

Le firmware doit être publié dans un registre OCI avec le media type correct.
La layer principale doit porter le media type `application/vnd.embewi.firmware.bin`
et les annotations chip/IDF :

```bash
# Exemple avec oras
oras push registry.local/embewi/wheel-controller:v1.1.0 \
  --artifact-type application/vnd.embewi.firmware.config.v1+json \
  wheel-controller.bin:application/vnd.embewi.firmware.bin \
  --annotation "embewi.io/chip=esp32c3" \
  --annotation "embewi.io/idf-version=v6.0.0"
```

Si les annotations sont absentes, le Core utilise les informations remontées
par `GET /info` sur le device cible.

---

## 2. Créer le McuDeployment

```bash
kubectl apply -f - <<EOF
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

`spec.nodeName` est la méthode recommandée (pin explicite). L'alternative
`spec.nodeSelector` accepte des labels McuNode mais doit résoudre exactement
un seul device — sinon le déploiement échoue en phase Binding.

---

## 3. Suivre la progression

```bash
# Suivi en temps réel
kubectl get mcudep wheel-controller-v1-1-0 -w
# NAME                       NODE               PHASE       AGE
# wheel-controller-v1-1-0   esp32-motor-left   Pulling     2s
# wheel-controller-v1-1-0   esp32-motor-left   Preparing   4s
# wheel-controller-v1-1-0   esp32-motor-left   Writing     5s
# wheel-controller-v1-1-0   esp32-motor-left   Activating  38s
# wheel-controller-v1-1-0   esp32-motor-left   Confirming  40s
# wheel-controller-v1-1-0   esp32-motor-left   Deployed    55s

# Attendre la disponibilité (compatible kubectl wait)
kubectl wait mcudeployment/wheel-controller-v1-1-0 \
  --for=condition=Available --timeout=5m
```

---

## 4. Pousser une configuration (McuConfigMap)

Avant ou après le firmware, on peut pousser une config runtime via un objet
`McuConfigMap` référencé dans le McuDeployment.

```bash
kubectl apply -f - <<EOF
apiVersion: embewi.io/v1alpha1
kind: McuConfigMap
metadata:
  name: wheel-left-gpio
  namespace: default
data:
  gpio_button: "9"
  gpio_ws2812: "48"
  ntp_server: "ntp.local"
EOF
```

Puis référencer dans le McuDeployment :

```yaml
spec:
  nodeName: esp32-motor-left
  firmware:
    image: registry.local/embewi/wheel-controller:v1.1.0
  configMapRef: wheel-left-gpio
```

Limites NVS à respecter :
- Clé : 15 caractères max
- Valeur : 63 caractères max

---

## 5. Diagnostiquer un déploiement Failed

```bash
kubectl describe mcudep wheel-controller-v1-1-0
# Status:
#   Phase:    Failed
#   Message:  [ConfirmTimeout] aucune confirmation dans les 2m0s après activate
```

| Message | Cause probable | Action |
|---------|----------------|--------|
| `NoDeviceMatched` | `spec.nodeName` incorrect | Vérifier `kubectl get mcunode` |
| `DeviceBusy` | Autre McuDeployment actif sur ce node | Attendre ou supprimer l'autre |
| `prepare refusé: chip_mismatch` | Firmware compilé pour un autre chip | Rebuilder pour `esp32c3` |
| `stream blob: GET blob → HTTP 404` | Image absente du registre | Vérifier le registre OCI |
| `clé "esp32-motor-left" absente du secret` | Token manquant | Ajouter la clé dans `embewi-tokens` |
| `ConfirmTimeout` | Device n'a pas rebooté ou rollback silencieux | Consulter les logs |
| `DeviceRollback` | Self-check KO sur le device | Consulter les logs + heartbeat state |

---

## 6. Relancer un déploiement Failed

Les phases `Failed` et `Deployed` sont terminales. Pour réessayer :

```bash
kubectl delete mcudep wheel-controller-v1-1-0
kubectl apply -f wheel-controller-v1-1-0.yaml
```
