# Enrôler un device

Un device ESP32 devient un `McuNode` après trois étapes : provisioning via le
portail captif, création du Secret token, et création de l'objet K8s.

---

## 1. Provisionner le device (premier boot)

Au premier boot (NVS vide), l'agent démarre en mode AP WiFi (`embewi-XXXX`) et
sert un portail captif sur `http://192.168.4.1`.

**Champs du formulaire :**

| Champ | Obligatoire | Description |
|-------|-------------|-------------|
| `ssid` | Oui | SSID du réseau WiFi cible |
| `pass` | Non | Mot de passe WiFi |
| `ctrl_url` | Oui | URL du Core (`https://<IP-du-pod>:8080`) |
| `ip` | Non¹ | IP statique du device (vide = DHCP) |
| `mask` | Non¹ | Masque réseau |
| `gw` | Non¹ | Passerelle |
| `token` | Non | Token Bearer — généré aléatoirement si vide² |

> ¹ `ip`, `mask` et `gw` sont tous obligatoires ensemble si IP statique.
>
> ² Le token généré est affiché **une seule fois** dans la page de confirmation
> — le noter immédiatement.

**Page de confirmation :**

```text
Configuration enregistrée ✓

Copiez ce token maintenant :
┌─────────────────────────────────────┐
│ a3f7c1b2e8d09441f6bc3e7a2c504d8f   │
└─────────────────────────────────────┘
Le device redémarre dans 15 secondes…
```

Après reboot, le device se connecte au WiFi et commence à envoyer des
heartbeats vers `ctrl_url`.

---

## 2. Créer le Secret token du device

Chaque device possède son **propre Secret** Kubernetes portant son token Bearer.
Le Secret est référencé dans `McuNodeSpec.TokenRef` (§1 contrat).

```bash
# Créer le Secret dédié à ce device :
kubectl create secret generic esp32-motor-left-token \
  --from-literal=token="a3f7c1b2e8d09441f6bc3e7a2c504d8f" \
  --namespace=default
```

Structure attendue du Secret :

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: esp32-motor-left-token
  namespace: default
type: Opaque
stringData:
  token: "a3f7c1b2e8d09441f6bc3e7a2c504d8f"   # token affiché au portail captif
```

> Le Secret doit être dans le **même namespace** que le McuNode (ou le namespace
> spécifié dans `spec.tokenRef.namespace`).

---

## 3. Créer le McuNode

```bash
kubectl apply -f - <<EOF
apiVersion: embewi.io/v1alpha1
kind: McuNode
metadata:
  name: esp32-motor-left
  namespace: default
  labels:
    role: motor
    side: left
spec:
  nodeId: esp32-motor-left
  tokenRef:
    name: esp32-motor-left-token   # Secret créé à l'étape 2
    namespace: default
EOF
```

`spec.nodeId` doit correspondre **exactement** à `EMBEWI_NODE_ID` compilé dans
le firmware — c'est la clé de réconciliation avec les heartbeats reçus.

---

## 4. Vérifier l'état

Dès que le device envoie son premier heartbeat, le status se peuple :

```bash
kubectl get mcunode esp32-motor-left
# NAME               STATUS    AGE   VERSION
# esp32-motor-left   running   30s   1.0.0

kubectl describe mcunode esp32-motor-left
# Status:
#   Ip:              192.168.10.50
#   State:           running
#   Last Heartbeat:  2026-06-29T10:00:03Z
#   Ready:           true
#   Conditions:
#     Type:    Provisioned  Status: True   Reason: ProvisioningComplete
#     Type:    Ready        Status: True   Reason: HeartbeatOK
```

Si `Ready` reste `false` après 15 secondes :
- Vérifier que `ctrl_url` pointe vers le pod Core (`kubectl get svc -n default`)
- Vérifier que `spec.nodeId` correspond exactement à `EMBEWI_NODE_ID` du firmware
- Vérifier que le Secret `esp32-motor-left-token` contient bien la clé `token`
- Consulter les logs du Core : `kubectl logs deployment/embewi-core`

---

## Rotation du token

Pour changer le token Bearer d'un device sans interruption de service :

```bash
# 1. Générer un nouveau token
NEW_TOKEN=$(openssl rand -hex 32)

# 2. Mettre à jour le Secret K8s
kubectl patch secret esp32-motor-left-token \
  --type=json \
  -p="[{\"op\":\"replace\",\"path\":\"/data/token\",\"value\":\"$(echo -n $NEW_TOKEN | base64)\"}]"

# 3. Le Core appelle POST /token avec l'ancien token pour notifier le device
#    (via kubectl exec ou l'API embewi-core — à implémenter selon vos besoins)
```

> La rotation atomique est garantie côté agent : le device commite le nouveau
> token en NVS avant de répondre — pas de fenêtre sans authentification.
