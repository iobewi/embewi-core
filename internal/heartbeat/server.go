// Package heartbeat expose le serveur HTTP qui reçoit les flux sortants des agents
// (POST /v1alpha1/heartbeat et POST /v1alpha1/logs, contrat §5).
// Il met à jour le status des McuNode K8s en conséquence.
package heartbeat

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"github.com/embewi/core/api/v1alpha1"
	"github.com/embewi/core/internal/metrics"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// HeartbeatPayload correspond au corps POST /v1alpha1/heartbeat (contrat §5).
// Champs requis : node_id, ip, ts, state, ota_validated, config_generation.
type HeartbeatPayload struct {
	NodeID          string  `json:"node_id"`
	IP              string  `json:"ip"`
	TS              int64   `json:"ts"`
	State           string  `json:"state"`
	DeploymentID    string  `json:"deployment_id"`
	FirmwareDigest  string  `json:"firmware_digest"`
	OtaValidated    bool    `json:"ota_validated"`
	UptimeMs        int64   `json:"uptime_ms"`
	HeapFree        int     `json:"heap_free"`
	RSSI            int     `json:"rssi"`
	ConfigGeneration int    `json:"config_generation"`
	TempCelsius     float64 `json:"temp_celsius"`
	TaskHwmMin      int     `json:"task_hwm_min"`
}

// LogPayload correspond au corps POST /v1alpha1/logs (contrat §5).
type LogPayload struct {
	TS       int64  `json:"ts"`
	Node     string `json:"node"`
	Workload string `json:"workload"`
	Level    string `json:"level"`
	Msg      string `json:"msg"`
}

// Server écoute sur addr et met à jour les McuNode via le client K8s.
// En production les devices ESP imposent HTTPS (§1 contrat) : configurer TLSCertFile/TLSKeyFile
// ou terminer TLS à l'ingress/proxy devant ce serveur.
type Server struct {
	addr        string
	client      client.Client
	TLSCertFile string // chemin PEM du certificat serveur (vide = HTTP plain)
	TLSKeyFile  string // chemin PEM de la clé privée
	TokenSecret string // nom du Secret K8s contenant les tokens Bearer (défaut : "embewi-tokens")
}

func New(addr string, c client.Client) *Server {
	return &Server{addr: addr, client: c}
}

// validateToken vérifie le Bearer token du heartbeat contre le Secret K8s référencé
// par node.Spec.TokenRef (§1 contrat). Comparaison temps-constant.
func (s *Server) validateToken(ctx context.Context, r *http.Request, node *v1alpha1.McuNode) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	provided := strings.TrimPrefix(auth, "Bearer ")

	// Résoudre le Secret : node.Spec.TokenRef prioritaire, fallback sur Secret centralisé.
	secretName := node.Spec.TokenRef.Name
	secretNS := node.Spec.TokenRef.Namespace
	if secretNS == "" {
		secretNS = node.Namespace
	}
	tokenKey := "token"
	if secretName == "" {
		// Fallback : Secret centralisé (clé = nodeId) — migration ou test.
		secretName = s.TokenSecret
		if secretName == "" {
			secretName = "embewi-tokens"
		}
		secretNS = node.Namespace
		tokenKey = node.Spec.NodeID
	}

	var secret corev1.Secret
	if err := s.client.Get(ctx, client.ObjectKey{Name: secretName, Namespace: secretNS}, &secret); err != nil {
		log.FromContext(ctx).Error(err, "lecture Secret token échouée", "secret", secretName)
		return false
	}
	expected, ok := secret.Data[tokenKey]
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(expected))), []byte(provided)) == 1
}

// Handler retourne le http.Handler du serveur heartbeat.
// Exposé pour les tests : httptest.NewServer(srv.Handler()).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/v1alpha1/logs", s.handleLog)
	return mux
}

func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:         s.addr,
		Handler:      s.Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("heartbeat listen %s: %w", s.addr, err)
	}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	if s.TLSCertFile != "" && s.TLSKeyFile != "" {
		log.FromContext(ctx).Info("heartbeat server started (TLS)", "addr", s.addr)
		if err := srv.ServeTLS(ln, s.TLSCertFile, s.TLSKeyFile); err != nil && err != http.ErrServerClosed {
			return err
		}
	} else {
		log.FromContext(ctx).Info("heartbeat server started (plain HTTP — TLS recommandé en prod)", "addr", s.addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return err
		}
	}
	return nil
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var hb HeartbeatPayload
	if err := json.Unmarshal(body, &hb); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	logger := log.FromContext(r.Context()).WithValues("node_id", hb.NodeID, "state", hb.State)

	// Retrouve le McuNode par node_id (spec.nodeId).
	node, err := s.findNode(r.Context(), hb.NodeID)
	if err != nil {
		logger.Error(err, "McuNode introuvable pour ce node_id")
		// On répond 200 : l'agent ne doit pas crasher si le Core ne connaît pas encore ce node.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Validation Bearer token — comparaison temps-constant (§1 contrat).
	if !s.validateToken(r.Context(), r, node) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// IP du device : contrat §5/§8 — utiliser heartbeat.ip, pas RemoteAddr.
	// Fallback sur RemoteAddr si le champ est absent (compatibilité).
	sourceIP := hb.IP
	if sourceIP == "" {
		sourceIP, _, _ = net.SplitHostPort(r.RemoteAddr)
	}

	now := metav1.NewTime(time.Now())
	ready := hb.State == "running" && hb.OtaValidated

	patch := client.MergeFrom(node.DeepCopy())
	node.Status.IP             = sourceIP
	node.Status.State          = hb.State
	node.Status.FirmwareDigest = hb.FirmwareDigest
	node.Status.DeploymentID   = hb.DeploymentID
	node.Status.OtaValidated   = hb.OtaValidated
	node.Status.HeapFree       = hb.HeapFree
	node.Status.RSSI           = hb.RSSI
	node.Status.UptimeMs       = hb.UptimeMs
	node.Status.ConfigGeneration = hb.ConfigGeneration
	node.Status.TaskHwmMin     = hb.TaskHwmMin
	node.Status.Ready          = ready
	node.Status.LastHeartbeat  = &now

	// temp_celsius : filtrer la sentinelle -127.0 (capteur SoC indisponible).
	if hb.TempCelsius != -127.0 {
		node.Status.TempCelsius = hb.TempCelsius
	}

	// Conditions §8a : Provisioned + Ready mis à jour à chaque heartbeat reçu.
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:    "Provisioned",
		Status:  metav1.ConditionTrue,
		Reason:  "ProvisioningComplete",
		Message: "heartbeat reçu, node_id et token établis",
	})
	if ready {
		apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "HeartbeatOK",
			Message: fmt.Sprintf("heartbeat reçu, state=%s", hb.State),
		})
	} else {
		// Heartbeat reçu mais device pas encore prêt (pending_verify, degraded…).
		// Raison distincte de HeartbeatTimeout pour différencier device vivant vs silencieux.
		apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "DeviceNotReady",
			Message: fmt.Sprintf("heartbeat reçu, state=%s (non prêt)", hb.State),
		})
	}

	if err := s.client.Status().Patch(r.Context(), node, patch); err != nil {
		logger.Error(err, "patch McuNode status failed")
		// Métriques non mises à jour si le patch K8s échoue — évite la divergence K8s/Prometheus.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Métriques §8b : mise à jour des gauges uniquement après patch K8s réussi.
	metrics.UpdateFromHeartbeat(metrics.HeartbeatData{
		NodeID:           hb.NodeID,
		Workload:         hb.DeploymentID,
		Chip:             node.Status.Chip,
		HeapFree:         hb.HeapFree,
		RSSI:             hb.RSSI,
		UptimeMs:         hb.UptimeMs,
		TempCelsius:      hb.TempCelsius,
		TaskHwmMin:       hb.TaskHwmMin,
		ConfigGeneration: hb.ConfigGeneration,
		TS:               hb.TS,
		OtaValidated:     hb.OtaValidated,
	})

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2048))

	var entry LogPayload
	if err := json.Unmarshal(body, &entry); err != nil {
		w.WriteHeader(http.StatusOK) // on absorbe les entrées malformées
		return
	}

	logger := log.FromContext(r.Context())
	switch entry.Level {
	case "fatal", "error":
		logger.Error(nil, entry.Msg, "node", entry.Node, "workload", entry.Workload)
	default:
		logger.Info(entry.Msg, "node", entry.Node, "workload", entry.Workload, "level", entry.Level)
	}

	w.WriteHeader(http.StatusOK)
}

// findNode cherche un McuNode dont spec.nodeId == nodeID dans tous les namespaces.
func (s *Server) findNode(ctx context.Context, nodeID string) (*v1alpha1.McuNode, error) {
	var list v1alpha1.McuNodeList
	if err := s.client.List(ctx, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.NodeID == nodeID {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no McuNode with nodeId=%q", nodeID)
}

// FindByNodeID est exporté pour le McuDeployment controller (binding).
func (s *Server) FindByNodeID(ctx context.Context, nodeID string) (*v1alpha1.McuNode, error) {
	return s.findNode(ctx, nodeID)
}

// NodeKey retourne le types.NamespacedName d'un McuNode par son nodeId.
func NodeKey(ctx context.Context, c client.Client, nodeID string) (types.NamespacedName, error) {
	var list v1alpha1.McuNodeList
	if err := c.List(ctx, &list); err != nil {
		return types.NamespacedName{}, err
	}
	for _, n := range list.Items {
		if n.Spec.NodeID == nodeID {
			return types.NamespacedName{Name: n.Name, Namespace: n.Namespace}, nil
		}
	}
	return types.NamespacedName{}, fmt.Errorf("no McuNode with nodeId=%q", nodeID)
}
