// Package heartbeat expose le serveur HTTP qui reçoit les flux sortants des agents
// (POST /v1alpha1/heartbeat et POST /v1alpha1/logs, contrat §5).
// Il met à jour le status des McuNode K8s en conséquence.
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/embewi/core/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// HeartbeatPayload correspond au corps POST /v1alpha1/heartbeat (contrat §5).
type HeartbeatPayload struct {
	NodeID         string `json:"node_id"`
	TS             int64  `json:"ts"`
	State          string `json:"state"`
	DeploymentID   string `json:"deployment_id"`
	FirmwareDigest string `json:"firmware_digest"`
	OtaValidated   bool   `json:"ota_validated"`
	UptimeMs       int64  `json:"uptime_ms"`
	HeapFree       int    `json:"heap_free"`
	RSSI           int    `json:"rssi"`
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
type Server struct {
	addr   string
	client client.Client
}

func New(addr string, c client.Client) *Server {
	return &Server{addr: addr, client: c}
}

func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/v1alpha1/logs", s.handleLog)

	srv := &http.Server{
		Addr:         s.addr,
		Handler:      mux,
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

	log.FromContext(ctx).Info("heartbeat server started", "addr", s.addr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
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

	// IP source du device (pour EndpointSlice).
	sourceIP, _, _ := net.SplitHostPort(r.RemoteAddr)

	now := metav1.NewTime(time.Now())
	ready := hb.State == "running" && hb.OtaValidated

	patch := client.MergeFrom(node.DeepCopy())
	node.Status.IP            = sourceIP
	node.Status.State         = hb.State
	node.Status.FirmwareDigest = hb.FirmwareDigest
	node.Status.DeploymentID  = hb.DeploymentID
	node.Status.OtaValidated  = hb.OtaValidated
	node.Status.HeapFree      = hb.HeapFree
	node.Status.RSSI          = hb.RSSI
	node.Status.UptimeMs      = hb.UptimeMs
	node.Status.Ready         = ready
	node.Status.LastHeartbeat = &now

	if err := s.client.Status().Patch(r.Context(), node, patch); err != nil {
		logger.Error(err, "patch McuNode status failed")
	}

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
