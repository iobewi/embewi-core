// Package agent fournit un client HTTP vers l'API inbound de l'agent ESP (contrat §4).
// Transport : HTTPS, skip verify (cert auto-signé fallback ou cert-manager interne).
// Auth    : Bearer token par node.
package agent

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	apiPrefix   = "/v1alpha1"
	httpTimeout = 10 * time.Second

	// SupportedAPIVersion est l'unique version de protocole supportée par ce Core.
	SupportedAPIVersion = "v1alpha1"
)

// ErrUnsupportedAPIVersion signale qu'aucune version commune n'a été trouvée
// entre celles annoncées par le device (InfoResponse.ApiVersions) et celles
// supportées par ce Core (contrat §4, découverte de version d'API).
var ErrUnsupportedAPIVersion = errors.New("aucune version d'API commune avec le device")

// NegotiateAPIVersion applique la règle du contrat §4 : le Core choisit la
// version la plus élevée qu'il supporte ET que le device annonce dans
// api_versions. Champ absent (device antérieur à cette révision du contrat)
// → v1alpha1 est supposé.
func NegotiateAPIVersion(versions []string) (string, error) {
	if len(versions) == 0 {
		return SupportedAPIVersion, nil
	}
	for _, v := range versions {
		if v == SupportedAPIVersion {
			return SupportedAPIVersion, nil
		}
	}
	return "", fmt.Errorf("%w: device annonce %v, Core ne supporte que %q", ErrUnsupportedAPIVersion, versions, SupportedAPIVersion)
}

// Client appelle l'API HTTPS de l'agent sur un device ESP.
type Client struct {
	baseURL    string // ex: "https://192.168.10.50"
	token      string
	http       *http.Client // appels rapides : timeout 10 s
	httpStream *http.Client // streaming OTA : sans timeout global (piloté par le contexte)
}

func New(ip, token string) *Client {
	// Transport partagé entre les deux clients pour réutiliser les connexions TLS.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // cert auto-signé agent
	}
	return &Client{
		baseURL: "https://" + ip,
		token:   token,
		http: &http.Client{
			Timeout:   httpTimeout,
			Transport: transport,
		},
		httpStream: &http.Client{
			// Pas de Timeout : http.Client.Timeout couvre l'upload complet.
			// Un firmware de 1 MB sur Wi-Fi congestionné dépasse largement 10 s.
			Transport: transport,
		},
	}
}

// doWith exécute la requête. contentLength >= 0 force req.ContentLength — requis
// pour tout body dont le type concret n'est pas reconnu par http.NewRequest
// (*bytes.Reader/*bytes.Buffer/*strings.Reader) : sans ça, Go bascule en
// Transfer-Encoding: chunked et ignore silencieusement un header Content-Length
// posé à la main (net/http ne l'écrit jamais depuis la map Header côté requête).
func (c *Client) doWith(hc *http.Client, method, path string, body io.Reader, contentLength int64, extraHeaders map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.baseURL+apiPrefix+path, body)
	if err != nil {
		return nil, err
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return hc.Do(req)
}

func (c *Client) do(method, path string, body io.Reader, extraHeaders map[string]string) (*http.Response, error) {
	return c.doWith(c.http, method, path, body, -1, extraHeaders)
}

// InfoResponse correspond au GET /v1alpha1/info.
type InfoResponse struct {
	NodeID string `json:"node_id"`
	// ApiVersions liste les versions de protocole supportées par l'agent,
	// la plus élevée en premier (contrat §4). Absent chez les devices
	// antérieurs à cette révision du contrat — cf. NegotiateAPIVersion.
	ApiVersions     []string `json:"api_versions,omitempty"`
	Chip            string   `json:"chip"`
	IDFVersion      string   `json:"idf_version"`
	FlashSize       int64    `json:"flash_size"`
	RAMSize         int64    `json:"ram_size"`
	PartitionLayout string   `json:"partition_layout"`
	ActiveSlot      string   `json:"active_slot"`
	Firmware        struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Digest  string `json:"digest"`
	} `json:"firmware"`
	Staged struct {
		State        string `json:"state"`
		Slot         string `json:"slot"`
		Digest       string `json:"digest"`
		DeploymentID string `json:"deployment_id"`
	} `json:"staged"`
	State            string `json:"state"`
	AppPort          int    `json:"app_port"`
	ConfigGeneration int    `json:"config_generation"`
}

func (c *Client) GetInfo() (*InfoResponse, error) {
	resp, err := c.do(http.MethodGet, "/info", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /info → %d", resp.StatusCode)
	}
	var info InfoResponse
	return &info, json.NewDecoder(resp.Body).Decode(&info)
}

// HealthResponse correspond au GET /v1alpha1/health (optionnel — §4 contrat).
type HealthResponse struct {
	Status string `json:"status"` // "ok" ou "degraded"
}

func (c *Client) GetHealth() (*HealthResponse, error) {
	resp, err := c.do(http.MethodGet, "/health", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /health → %d", resp.StatusCode)
	}
	var h HealthResponse
	return &h, json.NewDecoder(resp.Body).Decode(&h)
}

// PrepareRequest correspond au POST /v1alpha1/ota/prepare.
type PrepareRequest struct {
	DeploymentID    string `json:"deployment_id"`
	Artifact        string `json:"artifact"` // référence OCI complète (image:tag)
	Digest          string `json:"digest"`
	Size            int64  `json:"size"`
	Chip            string `json:"chip"`
	IDFVersion      string `json:"idf_version"`
	PartitionLayout string `json:"partition_layout"`
}

type PrepareResponse struct {
	Accepted   bool   `json:"accepted"`
	TargetSlot string `json:"target_slot"`
	Reason     string `json:"reason"`
}

func (c *Client) OTAPrepare(req PrepareRequest) (*PrepareResponse, error) {
	body, _ := json.Marshal(req)
	resp, err := c.do(http.MethodPost, "/ota/prepare", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST /ota/prepare → %d", resp.StatusCode)
	}
	var out PrepareResponse
	return &out, json.NewDecoder(resp.Body).Decode(&out)
}

// OTAWriteError représente un refus métier de PUT /ota/write (§4b contrat).
// Permet au controller d'émettre le bon Event K8s sans parser des strings d'erreur.
type OTAWriteError struct {
	// Status : code agent (digest_mismatch, write_failed, ota_begin_failed, range_mismatch).
	Status string
	// HTTPStatus : code HTTP si l'erreur vient d'un 4xx/5xx ; 0 pour une erreur métier (HTTP 200).
	HTTPStatus int
}

func (e *OTAWriteError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("PUT /ota/write HTTP %d: %s", e.HTTPStatus, e.Status)
	}
	return "PUT /ota/write: " + e.Status
}

const (
	// otaChunkSize : taille de plage Content-Range (§4 contrat — write reprenable).
	otaChunkSize = 64 * 1024
	// otaChunkMaxAttempts : tentatives max par plage avant abandon (erreur réseau
	// transitoire ou resync 416 sans progression) — au-delà, le firmware entier est
	// re-tenté par l'appelant (reconcile suivant), cf. reprise inter-reboot [RÉSERVE].
	otaChunkMaxAttempts = 3
)

// OTAWrite streame le binaire vers PUT /v1alpha1/ota/write par plages Content-Range de
// otaChunkSize octets (§4 contrat — write reprenable, in-session). Une coupure réseau
// sur une plage retente cette même plage — déjà bufferisée en mémoire, jamais relue
// depuis firmware — jusqu'à otaChunkMaxAttempts fois, sans retransmettre les plages déjà
// confirmées par le device. C'est le principal gain par rapport à l'ancien PUT
// monolithique où toute coupure retransmettait 100% du binaire.
//
// Retourne *OTAWriteError pour les échecs métier (§4b) afin que le controller
// puisse émettre le bon Event K8s.
func (c *Client) OTAWrite(deploymentID, digest string, size int64, firmware io.Reader) error {
	buf := make([]byte, otaChunkSize)
	var start int64
	for start < size {
		end := start + otaChunkSize - 1
		if end >= size {
			end = size - 1
		}
		chunkLen := int(end - start + 1)
		if _, err := io.ReadFull(firmware, buf[:chunkLen]); err != nil {
			return fmt.Errorf("lecture firmware offset %d: %w", start, err)
		}
		if err := c.putOTAChunk(deploymentID, digest, buf[:chunkLen], start, end, size); err != nil {
			return err
		}
		start = end + 1
	}
	return nil
}

// putOTAChunk envoie la plage [start,end] (bornes incluses, sur un total de size octets)
// et ne retourne nil qu'une fois cette plage confirmée écrite par le device. `chunk` fait
// exactement end-start+1 octets.
func (c *Client) putOTAChunk(deploymentID, digest string, chunk []byte, start, end, size int64) error {
	offset := start    // début de la sous-plage restant à envoyer (avance sur resync 416)
	remaining := chunk // toujours chunk[offset-start:] — jamais relu depuis firmware

	for attempt := 0; attempt < otaChunkMaxAttempts; attempt++ {
		resp, err := c.doWith(c.httpStream, http.MethodPut, "/ota/write",
			bytes.NewReader(remaining), int64(len(remaining)), map[string]string{
				"Content-Type":           "application/octet-stream",
				"Content-Range":          fmt.Sprintf("bytes %d-%d/%d", offset, end, size),
				"X-Embewi-Deployment-Id": deploymentID,
				"X-Embewi-Digest":        digest,
			})
		if err != nil {
			continue // erreur réseau transitoire : retenter la même plage bufferisée
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			var result struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			if result.Status == "written" || result.Status == "partial" {
				return nil // plage [start,end] confirmée écrite par le device
			}
			status := result.Status
			if status == "" {
				status = result.Error
			}
			return &OTAWriteError{Status: status} // refus métier (§4b) : pas de retry
		case http.StatusRequestedRangeNotSatisfiable: // 416 — resync (§4 contrat)
			var result struct {
				Written int64 `json:"written"`
			}
			json.Unmarshal(raw, &result) //nolint:errcheck
			if result.Written < offset || result.Written > end+1 {
				// Le device rapporte un offset hors de la plage envoyée : pas de resync
				// possible sans re-transférer depuis firmware (RÉSERVE, cf. doc contrat).
				return &OTAWriteError{Status: "range_mismatch", HTTPStatus: http.StatusRequestedRangeNotSatisfiable}
			}
			if result.Written == end+1 {
				return nil // déjà intégralement appliquée (réponse précédente perdue en route)
			}
			remaining = chunk[result.Written-start:]
			offset = result.Written
		case http.StatusInternalServerError: // 500 — ota_begin_failed
			return &OTAWriteError{Status: "ota_begin_failed", HTTPStatus: http.StatusInternalServerError}
		default:
			return fmt.Errorf("PUT /ota/write → %d: %s", resp.StatusCode, raw)
		}
	}
	return fmt.Errorf("PUT /ota/write plage %d-%d: abandon après %d tentatives", start, end, otaChunkMaxAttempts)
}

// ConfigResponse correspond au GET /v1alpha1/config (contrat §4).
type ConfigResponse struct {
	Generation       int               `json:"generation"`
	ActiveGeneration int               `json:"active_generation"`
	Active           map[string]string `json:"active"`
	NVS              map[string]string `json:"nvs"`
}

func (c *Client) GetConfig() (*ConfigResponse, error) {
	resp, err := c.do(http.MethodGet, "/config", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /config → %d", resp.StatusCode)
	}
	var cfg ConfigResponse
	return &cfg, json.NewDecoder(resp.Body).Decode(&cfg)
}

// PostConfig pousse un jeu de clés/valeurs vers le NVS de l'agent (merge-on-key, §4a).
func (c *Client) PostConfig(data map[string]string) error {
	body, _ := json.Marshal(map[string]interface{}{"data": data})
	resp, err := c.do(http.MethodPost, "/config", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /config → %d: %s", resp.StatusCode, raw)
	}
	return nil
}

// PostReboot déclenche un reboot contrôlé (§4 contrat).
// À utiliser après POST /config sans cycle OTA pour appliquer la config.
func (c *Client) PostReboot() error {
	body, _ := json.Marshal(map[string]interface{}{})
	resp, err := c.do(http.MethodPost, "/reboot", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /reboot → %d", resp.StatusCode)
	}
	return nil
}

type ActivateRequest struct {
	DeploymentID string `json:"deployment_id"`
	Reboot       bool   `json:"reboot"`
}

func (c *Client) OTAActivate(deploymentID string) error {
	body, _ := json.Marshal(ActivateRequest{DeploymentID: deploymentID, Reboot: true})
	resp, err := c.do(http.MethodPost, "/ota/activate", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /ota/activate → %d", resp.StatusCode)
	}
	return nil
}

// RotateToken envoie un nouveau token au device (§4 contrat — rotation sans coupure).
// Protocole : POST /token avec oldToken en Authorization, newToken dans le corps.
// Le device commite en NVS avant de répondre : atomique, pas de fenêtre sans auth.
// Contrainte contrat : 8 ≤ len(newToken) ≤ 64.
// Après un appel réussi, créer un nouveau Client avec newToken pour les appels suivants.
func (c *Client) RotateToken(newToken string) error {
	body, _ := json.Marshal(map[string]string{"token": newToken})
	resp, err := c.do(http.MethodPost, "/token", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /token → %d: %s", resp.StatusCode, raw)
	}
	return nil
}

// PostAppPort reconfigure le port TCP du service applicatif embarqué (§4 contrat).
// Persisté en NVS, effectif au prochain démarrage de l'app (pas de reboot device requis).
// Contrainte contrat : 1024 ≤ port ≤ 65535.
func (c *Client) PostAppPort(port int) error {
	if port < 1024 || port > 65535 {
		return fmt.Errorf("port %d hors limites contrat (1024-65535)", port)
	}
	body, _ := json.Marshal(map[string]int{"port": port})
	resp, err := c.do(http.MethodPost, "/app/port", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /app/port → %d: %s", resp.StatusCode, raw)
	}
	return nil
}

// PostTLSCert pousse un certificat TLS + clé privée PEM pour le serveur HTTPS de
// l'agent (§4 contrat) — rotation de certificat sans cycle OTA. Effectif au prochain
// embewi_http_start() (après reboot).
func (c *Client) PostTLSCert(certPEM, keyPEM string) error {
	body, _ := json.Marshal(map[string]string{"cert_pem": certPEM, "key_pem": keyPEM})
	resp, err := c.do(http.MethodPost, "/tls/cert", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /tls/cert → %d: %s", resp.StatusCode, raw)
	}
	return nil
}
