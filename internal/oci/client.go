// Package oci implémente un client OCI Distribution Spec (RFC 7235) pour pull
// les artefacts firmware depuis un registre OCI (Docker Hub, Harbor, Zot, etc.).
// Pas de dépendance externe — utilise uniquement net/http.
package oci

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
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
)

// Client est un client OCI léger : résolution de manifeste + pull de blob.
type Client struct {
	http          *http.Client
	username      string
	password      string
	plainHTTP     map[string]bool // registres à contacter en HTTP plain (ex: Zot local)
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

// WithPlainHTTPRegistries liste les registres à contacter en HTTP plain (ex: Zot NodePort local).
// Format : "host:port" ou "host", séparés par des virgules.
func WithPlainHTTPRegistries(registries ...string) Option {
	return func(c *Client) {
		if c.plainHTTP == nil {
			c.plainHTTP = make(map[string]bool)
		}
		for _, r := range registries {
			c.plainHTTP[r] = true
		}
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

	// Cherche la layer firmware typée.
	for _, layer := range manifest.Layers {
		if layer.MediaType == MediaTypeFirmwareBin {
			return firmwareMetaFromLayer(layer, manifest.Annotations), nil
		}
	}

	// Fallback : première layer disponible (push simplifié sans mediaType explicite).
	if len(manifest.Layers) > 0 {
		return firmwareMetaFromLayer(manifest.Layers[0], manifest.Annotations), nil
	}

	return nil, fmt.Errorf("aucune layer trouvée dans %q", image)
}

// StreamBlob ouvre un stream HTTP vers le blob identifié par digest.
// L'appelant est responsable de fermer le ReadCloser retourné.
func (c *Client) StreamBlob(ctx context.Context, image, digest string) (io.ReadCloser, error) {
	r, err := parseRef(image)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", c.scheme(r.registry), r.registry, r.repo, digest)
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
	return resp.Body, nil
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

// scheme retourne le schéma HTTP à utiliser pour ce registre.
func (c *Client) scheme(registry string) string {
	if c.plainHTTP[registry] {
		return "http"
	}
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
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.scheme(r.registry), r.registry, r.repo, r.tag)
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
