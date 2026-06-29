# Enrôler un device

Un device ESP32 devient un `McuNode` après trois étapes : provisioning via le
portail captif, création de l'objet K8s, et enregistrement du token Bearer.

---

## 1. Provisionner le device (premier boot)

Au premier boot (NVS vide), l'agent démarre en mode AP WiFi (`embewi-XXXX`) et
sert un portail captif sur `http://192.168.4.1`.

**Champs du formulaire :**

| Champ | Obligatoire | Description |
|-------|-------------|-------------|
| `ssid` | Oui | SSID du réseau WiFi cible |
| `pass` | Non | Mot de passe WiFi |
| `ctrl_url` | Oui | URL du Core (`http://<IP-du-pod>:8080`) |
| `ip` | Non¹ | IP statique du device (vide = DHCP) |
| `mask` | Non¹ | Masque réseau |
| `gw` | Non¹ | Passerelle — pré-remplie avec l'IP du Core |
| `token` | Non | Token Bearer — généré aléatoirement si vide² |

> ¹ `ip`, `mask` et `gw` sont tous obligatoires ensemble si IP statique.
>
> ² Le token généré est affiché **une seule fois** dans la page de confirmation
> — le noter immédiatement. C'est la seule occasion de le récupérer.

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

## 2. Enregistrer le token dans le Secret K8s

Le Core charge les tokens depuis un Secret Kubernetes. La clé est le `nodeId`
du device, la valeur est le token hex encodé en base64.

**Création du Secret (premier device) :**

```bash
kubectl create secret generic embewi-tokens \
  --from-literal=esp32-motor-left="a3f7c1b2e8d09441f6bc3e7a2c504d8f"
```

**Ajout d'un device à un Secret existant :**

```bash
kubectl patch secret embewi-tokens \
  --type=json \
  -p='[{"op":"add","path":"/data/esp32-motor-left","value":"'$(echo -n "a3f7c1b2e8d09441f6bc3e7a2c504d8f" | base64)'"}]'
```

> Le Secret doit être dans le **même namespace** que les McuDeployment.

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
EOF
```

`spec.nodeId` doit correspondre **exactement** à `EMBEWI_NODE_ID` compilé dans
le firmware — c'est la clé de réconciliation avec les heartbeats reçus.

---

## 4. Vérifier l'état

Dès que le device envoie son premier heartbeat, le status se peuple :

```bash
kubectl get mcunode esp32-motor-left
# NAME               STATE     READY   AGE
# esp32-motor-left   running   true    30s

kubectl describe mcunode esp32-motor-left
# Status:
#   Ip:              192.168.10.50
#   State:           running
#   Last Heartbeat:  2026-06-29T10:00:03Z
#   Ready:           true
```

Si `Ready` reste `false` après 30 secondes :
- Vérifier que `ctrl_url` pointe bien vers le pod Core (`kubectl get svc`)
- Vérifier que `spec.nodeId` correspond à `EMBEWI_NODE_ID` du firmware
- Consulter les logs du Core : `kubectl logs deployment/embewi-core`
