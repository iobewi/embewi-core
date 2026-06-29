# API agent (Core → ESP)

> API inbound de l'agent ESP32, telle qu'appelée par le Core.
> Le contrat de référence reste `embewi-contract-v2.md`.

Version protocole : `v1alpha1`
Transport : **HTTPS :443**
Authentification : `Authorization: Bearer <token>` sur tous les endpoints.

---

## `GET /v1alpha1/info`

Identité matérielle, firmware courant et slot stagé (clé de l'idempotence OTA).

**Réponse 200 :**

```json
{
  "node_id": "esp32-motor-left",
  "chip": "esp32c3",
  "idf_version": "v6.0.0",
  "flash_size": 4194304,
  "ram_size": 401408,
  "partition_layout": "embewi-ab-v1",
  "active_slot": "ota_0",
  "firmware": {
    "name": "wheel-controller",
    "version": "1.0.0",
    "digest": "sha256:a3f7c1..."
  },
  "staged": {
    "state": "none",
    "slot": "",
    "digest": "",
    "deployment_id": ""
  },
  "state": "running",
  "app_port": 8080
}
```

| Champ | Description |
|-------|-------------|
| `staged.state` | `none` \| `written` \| `activating` — clé d'idempotence OTA (§6 contrat) |
| `active_slot` | Partition active : `factory` \| `ota_0` \| `ota_1` |
| `app_port` | Port du service applicatif (NVS, défaut 8080) |

---

## `GET /v1alpha1/health`

État de santé local.

**Réponse 200 :**

```json
{
  "status": "ok",
  "state": "running",
  "checks": {
    "app":     "ok",
    "sensors": "ok",
    "storage": "ok"
  }
}
```

| `status` | Condition |
|----------|-----------|
| `ok` | `state == running` |
| `degraded` | `state == degraded` |
| `fail` | tout autre état |

---

## `POST /v1alpha1/ota/prepare`

Le Core annonce le firmware avant tout transfert. L'ESP valide la compatibilité
matérielle et réserve le slot inactif.

**Corps de la requête :**

```json
{
  "deployment_id": "wheel-controller-1.1.0",
  "digest":        "sha256:b9e4f2...",
  "size":          983040,
  "chip":          "esp32c3",
  "idf_version":   "v6.0.0",
  "partition_layout": "embewi-ab-v1"
}
```

**Réponse 200 — accepté :**

```json
{ "accepted": true, "target_slot": "ota_1", "reason": null }
```

**Réponse 200 — refusé :**

```json
{ "accepted": false, "target_slot": null, "reason": "chip_mismatch" }
```

| `reason` | Cause | Event K8s |
|----------|-------|-----------|
| `chip_mismatch` | `chip` != `CONFIG_IDF_TARGET` | `OTARejectedChip` |
| `layout_mismatch` | `partition_layout` != `embewi-ab-v1` | `OTARejectedLayout` |
| `idf_incompatible` | version IDF incompatible | `OTARejectedIdf` |
| `size_too_large` | `size` > taille partition inactive | `OTARejectedSize` |
| `busy` | OTA déjà en cours | `OTABusy` |

> Un refus métier légitime répond HTTP 200 avec le code dans le corps. Les
> `4xx/5xx` sont des erreurs de protocole.

---

## `PUT /v1alpha1/ota/write`

Stream du binaire `.bin` brut vers le slot inactif. L'ESP calcule le SHA-256 en
incrémental et compare au digest annoncé.

**Headers requis :**

```
Content-Type: application/octet-stream
Content-Length: <taille exacte>
X-Embewi-Deployment-Id: <deployment_id>
X-Embewi-Digest: sha256:<hex>
Authorization: Bearer <token>
```

**Corps :** binaire `.bin` brut (pas base64, pas multipart).

**Réponse 200 — succès :**

```json
{ "written": 983040, "digest": "sha256:b9e4f2...", "status": "written" }
```

**Réponse 200 — erreurs :**

| `status` | Cause | Event K8s |
|----------|-------|-----------|
| `digest_mismatch` | SHA-256 final != digest annoncé | `OTADigestMismatch` |
| `write_failed` | Erreur d'écriture flash | `OTAWriteFailed` |

**Reprise sur `Content-Range` :**

```
Content-Range: bytes <start>-<end>/<total>
```

- `start=0` ou header absent → nouvelle session
- `start>0` aligné sur `written` → reprise (CONTINUE)
- `start>0` désaligné → `416` avec `{"error":"range_mismatch","written":N}`

---

## `POST /v1alpha1/ota/activate`

Configure le prochain boot sur le slot écrit et redémarre.

**Corps de la requête :**

```json
{ "deployment_id": "wheel-controller-1.1.0", "reboot": true }
```

**Réponse 200 (avant reboot) :**

```json
{ "status": "rebooting", "target_slot": "ota_1" }
```

Après reboot, l'agent est en `pending_verify` (15 s). Si le self-check passe →
`running` + `ota_validated=true`. Sinon → rollback bootloader automatique.

---

## `GET /v1alpha1/config`

Retourne la configuration NVS courante du device.

**Réponse 200 :**

```json
{
  "config_generation": 2,
  "data": {
    "gpio_button": "9",
    "gpio_ws2812": "48",
    "ntp_server": "ntp.local"
  }
}
```

---

## `POST /v1alpha1/config`

Pousse une configuration runtime. L'agent stocke en NVS et incrémente
`config_generation`. Les clés inconnues sont ignorées silencieusement.

**Corps de la requête :**

```json
{
  "config_generation": 3,
  "data": {
    "gpio_button": "9",
    "gpio_ws2812": "48"
  }
}
```

Limites NVS à respecter (validées côté Core avant push) :
- Clé : 15 caractères max
- Valeur : 63 caractères max

**Réponse 200 :**

```json
{ "status": "ok", "config_generation": 3 }
```

---

## `POST /v1alpha1/token`

Rotation du token Bearer. L'agent commite en NVS avant de répondre — atomicité
garantie.

**Corps de la requête :**

```json
{ "token": "<newToken>" }
```

**Réponse 200 :**

```json
{ "status": "ok" }
```

Séquence complète de rotation :

```text
1. Générer newToken, mettre à jour le Secret K8s.
2. POST /token (Bearer oldToken) {"token":"newToken"}
3. Device commite en NVS → seul newToken valide dès maintenant.
4. Core bascule sur newToken pour tous les appels suivants.
```

---

## Codes d'erreur HTTP

| Code | Signification |
|------|---------------|
| 200 | Succès (y compris les refus métier avec `accepted: false`) |
| 400 | Body malformé ou champ manquant |
| 401 | Token absent ou invalide |
| 413 | Body trop grand |
| 500 | Erreur interne ESP (NVS, OTA) |
