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

Retourne un `io.ReadCloser` — le blob est streamé directement vers l'ESP sans
buffer en mémoire. La taille des firmwares (typiquement ~1 Mo) ne justifie pas
un buffer intermédiaire.

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
