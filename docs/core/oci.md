# Client OCI

Package `internal/oci`. Utilise l'OCI Distribution Spec HTTP directement, sans
bibliothèque externe lourde.

## Résolution de manifeste

```
GET https://<registry>/v2/<repo>/manifests/<tag>
Accept: application/vnd.oci.image.manifest.v1+json,
        application/vnd.oci.artifact.manifest.v1+json,
        application/vnd.docker.distribution.manifest.v2+json
```

Extrait la layer `application/vnd.embewi.firmware.bin`. Si absente, prend la
première layer (fallback permissif pour push simplifié).

Annotations lues (sur la layer, ou sur le manifeste en fallback) :

- `embewi.io/chip` — ex: `esp32c3`
- `embewi.io/idf-version` — ex: `v6.0.0`

## Stream de blob

```
GET https://<registry>/v2/<repo>/blobs/sha256:<hex>
```

Retourne un `*oci.BlobStream` (implémente `io.ReadCloser`) — le blob est
streamé directement vers l'ESP sans buffer en mémoire. La taille des
firmwares (typiquement ~1 Mo) ne justifie pas un buffer intermédiaire.

`BlobStream` re-hache les octets au fil de la lecture et les compare au
digest attendu (§1/§9 contrat — `ResolveFirmware` se contente de lire le
digest *déclaré* par le registre, sans jamais le recalculer ; `StreamBlob`
referme cette boucle). Appeler `Err()` **après** avoir intégralement consommé
le flux pour connaître le résultat — `nil` si la lecture n'est pas allée
jusqu'au bout ou si le digest correspond, `oci.ErrBlobDigestMismatch` sinon.

`Close()` ne porte jamais cette erreur : le flux sert typiquement de corps à
une requête HTTP sortante (`PUT /ota/write`), et le transport `net/http`
appelle `Close()` lui-même après émission — un `Close()` en erreur y ferait
échouer tout le round-trip, y compris une réponse déjà reçue avec succès. Le
controller (`phaseWriting`, `mcudeployment_controller.go`) illustre l'usage :
`OTAWrite` d'abord, `Close()` ensuite (ignoré), puis `Err()`.

## Schéma attendu du manifeste OCI

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.embewi.firmware.config.v1+json"
  },
  "layers": [
    {
      "mediaType": "application/vnd.embewi.firmware.bin",
      "digest": "sha256:b9e4f2...",
      "size": 983040,
      "annotations": {
        "embewi.io/chip": "esp32c3",
        "embewi.io/idf-version": "v6.0.0"
      }
    }
  ]
}
```

## Protocole HTTP vs HTTPS

| Registre | Protocole |
|----------|-----------|
| `localhost` ou `127.x.x.x` | HTTP automatique |
| Tout autre hôte | HTTPS |

Pour désactiver la vérification TLS (registre local auto-signé) :
`OCI_INSECURE_TLS=true`.

## Vérification de signature (rôle « efficacité », contrat §1/§9)

```text
Core verifies for efficiency.
Bootloader verifies for trust.
```

Le Core **peut** vérifier une signature Ed25519 détachée avant de transférer
un firmware, en plus de la validation du digest et de la compat chip/layout.
Cette vérification est un étage d'efficacité (fail-fast avant OTA) — le
bootloader Secure Boot v2 reste la seule racine de confiance réelle côté
device (§1 contrat).

Activation : `OCI_TRUSTED_PUBLIC_KEY` = clé publique Ed25519 brute (32
octets), encodée en base64. Absente = vérification désactivée (posture
dev/MVP, comme `OCI_INSECURE_TLS`).

Le manifeste doit alors porter une annotation `embewi.io/signature` :
signature Ed25519 (base64) sur le digest de la layer firmware (le champ
`digest` de la layer `application/vnd.embewi.firmware.bin`, pas le digest du
manifeste lui-même) :

```json
{
  "layers": [{
    "mediaType": "application/vnd.embewi.firmware.bin",
    "digest": "sha256:b9e4f2..."
  }],
  "annotations": {
    "embewi.io/signature": "<base64 ed25519.Sign(privKey, []byte(\"sha256:b9e4f2...\"))>"
  }
}
```

Signer une image (hors périmètre Core — outillage de publication) :

```go
sig := ed25519.Sign(privKey, []byte(layerDigest))
annotation := base64.StdEncoding.EncodeToString(sig)
```

Si `OCI_TRUSTED_PUBLIC_KEY` est configurée, `ResolveFirmware` échoue avec
`oci.ErrSignatureMissing` (annotation absente) ou `oci.ErrSignatureInvalid`
(signature présente mais invalide, ou signée sur un autre digest — cas d'un
manifeste trafiqué après signature).

Cette vérification porte sur ce que le manifeste *déclare*. Combinée au
re-hash du blob dans `StreamBlob` (ci-dessus), un registre compromis qui sert
un blob substitué sous un digest annoncé identique à celui signé ne peut
plus passer inaperçu : la signature protège le digest déclaré, le re-hash
protège la correspondance digest↔octets réels.
