// Package oci implémente un client OCI Distribution Spec (RFC 7235) pour pull
// les artefacts firmware depuis un registre OCI (Docker Hub, Harbor, Zot, etc.).
// Pas de dépendance externe — utilise uniquement net/http.
package oci

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	mediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeOCIArtifact = "application/vnd.oci.artifact.manifest.v1+json"
	mediaTypeDocker2     = "application/vnd.docker.distribution.manifest.v2+json"
	// MediaTypeFirmwareBin est le mediaType attendu pour la layer binaire ESP.
	MediaTypeFirmwareBin = "application/vnd.embewi.firmware.bin"

	// signatureAnnotation porte la signature détachée Ed25519 (base64) du digest de la
	// layer firmware, au niveau du manifeste (§1/§9 contrat — rôle « efficacité » du Core).
	signatureAnnotation = "embewi.io/signature"
)

// ErrSignatureMissing : une clé de confiance est configurée (WithTrustedPublicKey) mais le
// manifeste ne porte pas l'annotation de signature — image non signée, refusée.
var ErrSignatureMissing = errors.New("annotation de signature OCI absente du manifeste")

// ErrSignatureInvalid : la signature présente ne vérifie pas contre la clé de confiance.
var ErrSignatureInvalid = errors.New("signature OCI invalide")

// ErrBlobDigestMismatch : le blob réellement streamé ne hache pas vers le digest annoncé
// par le descriptor du manifeste (§1/§9 contrat — rôle « efficacité » du Core). Ferme la
// boucle laissée ouverte par ResolveFirmware, qui ne fait que lire le digest déclaré par
// le registre sans jamais le recalculer depuis les octets réels.
var ErrBlobDigestMismatch = errors.New("digest du blob streamé ne correspond pas au digest attendu")

// Client est un client OCI léger : résolution de manifeste + pull de blob.
type Client struct {
	http     *http.Client
	username string
	password string

	// trustedPubKey : clé Ed25519 de confiance pour la vérification de signature
	// (§1 contrat, « Core verifies for efficiency »). nil = vérification désactivée
	// (posture dev/MVP, cf. WithInsecureTLS) — le bootloader Secure Boot v2 reste la
	// racine de confiance dans tous les cas.
	trustedPubKey ed25519.PublicKey
}

// Option configure un Client.
type Option func(*Client)

// WithBasicAuth active l'authentification Basic sur toutes les requêtes.
func WithBasicAuth(username, password string) Option {
	return func(c *Client) {
		c.username = username
		c.password = password
	}
}

// WithInsecureTLS désactive la vérification du certificat TLS (registres locaux).
func WithInsecureTLS() Option {
	return func(c *Client) {
		c.http.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // registre local auto-signé
		}
	}
}

// WithTrustedPublicKey active la vérification de signature (§1/§9 contrat) : toute
// image résolue doit porter une annotation de manifeste embewi.io/signature valide
// (Ed25519, signée sur le digest de la layer firmware), sinon ResolveFirmware échoue.
func WithTrustedPublicKey(pubKey ed25519.PublicKey) Option {
	return func(c *Client) {
		c.trustedPubKey = pubKey
	}
}

// New crée un Client OCI avec les options données.
func New(opts ...Option) *Client {
	c := &Client{
		http: &http.Client{Timeout: 120 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// FirmwareMeta contient les métadonnées extraites du manifeste OCI.
type FirmwareMeta struct {
	Digest     string // "sha256:<hex>" de la layer binaire
	Size       int64  // taille en octets du blob
	Chip       string // annotation "embewi.io/chip" (ex: esp32c3)
	IDFVersion string // annotation "embewi.io/idf-version"
}

// ResolveFirmware résout l'image OCI et retourne les métadonnées du firmware.
// Cherche la layer de mediaType application/vnd.embewi.firmware.bin ;
// accepte en fallback la première layer si le type n'est pas renseigné.
func (c *Client) ResolveFirmware(ctx context.Context, image string) (*FirmwareMeta, error) {
	r, err := parseRef(image)
	if err != nil {
		return nil, err
	}
	manifest, err := c.getManifest(ctx, r)
	if err != nil {
		return nil, err
	}

	var meta *FirmwareMeta
	// Cherche la layer firmware typée.
	for _, layer := range manifest.Layers {
		if layer.MediaType == MediaTypeFirmwareBin {
			meta = firmwareMetaFromLayer(layer, manifest.Annotations)
			break
		}
	}
	// Fallback : première layer disponible (push simplifié sans mediaType explicite).
	if meta == nil && len(manifest.Layers) > 0 {
		meta = firmwareMetaFromLayer(manifest.Layers[0], manifest.Annotations)
	}
	if meta == nil {
		return nil, fmt.Errorf("aucune layer trouvée dans %q", image)
	}

	if err := c.verifySignature(manifest.Annotations, meta.Digest); err != nil {
		return nil, fmt.Errorf("image %q: %w", image, err)
	}
	return meta, nil
}

// verifySignature applique le rôle « efficacité » du Core (§1/§9 contrat) : si une clé
// publique de confiance est configurée (WithTrustedPublicKey), le manifeste doit porter
// une annotation embewi.io/signature — signature Ed25519 (base64) sur le digest de la
// layer firmware — qui vérifie contre cette clé. Sans clé configurée, no-op (dev/MVP).
func (c *Client) verifySignature(annotations map[string]string, digest string) error {
	if c.trustedPubKey == nil {
		return nil
	}
	sigB64, ok := annotations[signatureAnnotation]
	if !ok {
		return ErrSignatureMissing
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("%w: annotation %s non décodable en base64", ErrSignatureInvalid, signatureAnnotation)
	}
	if !ed25519.Verify(c.trustedPubKey, []byte(digest), sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// StreamBlob ouvre un stream HTTP vers le blob identifié par digest, et vérifie au fil
// de l'eau que les octets réellement reçus correspondent à ce digest (§1/§9 contrat) —
// ResolveFirmware se contente de lire le digest déclaré par le registre, sans jamais le
// recalculer ; StreamBlob referme cette boucle.
//
// Le *BlobStream retourné est utilisable comme io.ReadCloser ordinaire. La vérification
// n'est concluante qu'une fois le flux intégralement consommé : appeler Err() après
// lecture complète pour savoir si le digest correspond. Close() ne fait que fermer la
// ressource sous-jacente — elle ne doit jamais retourner l'erreur de mismatch, car ce
// flux sert typiquement de corps à une requête HTTP sortante (PUT /ota/write) : le
// transport net/http appelle Close() automatiquement après émission, et un Close() en
// erreur y fait échouer tout le round-trip, y compris une réponse déjà reçue avec succès.
func (c *Client) StreamBlob(ctx context.Context, image, digest string) (*BlobStream, error) {
	r, err := parseRef(image)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme(r.registry), r.registry, r.repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET blob %s: %w", digest, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET blob %s → HTTP %d", digest, resp.StatusCode)
	}
	return &BlobStream{
		ReadCloser: resp.Body,
		hash:       sha256.New(),
		wantDigest: digest,
	}, nil
}

// BlobStream hache les octets lus au fil de l'eau et compare au digest attendu dès que
// le flux sous-jacent signale EOF. Voir StreamBlob pour pourquoi Close() ne porte pas
// cette erreur — utiliser Err() après consommation complète du flux.
type BlobStream struct {
	io.ReadCloser
	hash       hash.Hash
	wantDigest string
	read       int64
	mismatch   error
}

func (b *BlobStream) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.hash.Write(p[:n]) // hash.Hash.Write ne retourne jamais d'erreur (io.Writer contrat sha256)
		b.read += int64(n)
	}
	if err == io.EOF {
		got := "sha256:" + hex.EncodeToString(b.hash.Sum(nil))
		if got != b.wantDigest {
			b.mismatch = fmt.Errorf("%w: attendu %s, calculé %s (%d octets lus)",
				ErrBlobDigestMismatch, b.wantDigest, got, b.read)
		}
	}
	return n, err
}

// Err retourne ErrBlobDigestMismatch si le flux a été intégralement lu (EOF atteint) et
// que le hash calculé diverge du digest attendu. nil si la vérification n'a pas encore
// pu conclure (flux pas lu jusqu'au bout) ou si le digest correspond.
func (b *BlobStream) Err() error {
	return b.mismatch
}

// --- types manifeste OCI ---

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type ref struct {
	registry string
	repo     string
	tag      string
}

// parseRef décompose "registry/repo:tag" (ou "registry/path/repo:tag").
// Le registre est le premier segment (doit contenir un point ou un port).
func parseRef(image string) (ref, error) {
	parts := strings.SplitN(image, "/", 2)
	if len(parts) != 2 {
		return ref{}, fmt.Errorf("référence OCI invalide (format attendu: registre/repo:tag) : %q", image)
	}
	registry := parts[0]
	rest := parts[1]

	tag := "latest"
	if idx := strings.LastIndex(rest, ":"); idx != -1 {
		tag = rest[idx+1:]
		rest = rest[:idx]
	}

	return ref{registry: registry, repo: rest, tag: tag}, nil
}

// scheme retourne "http" pour les registres locaux (localhost, 127.x.x.x), "https" sinon.
func scheme(registry string) string {
	host := registry
	if h, _, err := net.SplitHostPort(registry); err == nil {
		host = h
	}
	if host == "localhost" || strings.HasPrefix(host, "127.") {
		return "http"
	}
	return "https"
}

func (c *Client) getManifest(ctx context.Context, r ref) (*manifest, error) {
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme(r.registry), r.registry, r.repo, r.tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join([]string{
		mediaTypeOCIManifest,
		mediaTypeOCIArtifact,
		mediaTypeDocker2,
	}, ", "))
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET manifeste %s/%s:%s: %w", r.registry, r.repo, r.tag, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET manifeste → HTTP %d: %s", resp.StatusCode, body)
	}

	var m manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("décodage manifeste: %w", err)
	}
	return &m, nil
}

func firmwareMetaFromLayer(l descriptor, manifestAnnotations map[string]string) *FirmwareMeta {
	chip := l.Annotations["embewi.io/chip"]
	idf := l.Annotations["embewi.io/idf-version"]
	if chip == "" {
		chip = manifestAnnotations["embewi.io/chip"]
	}
	if idf == "" {
		idf = manifestAnnotations["embewi.io/idf-version"]
	}
	return &FirmwareMeta{
		Digest:     l.Digest,
		Size:       l.Size,
		Chip:       chip,
		IDFVersion: idf,
	}
}
