// Package heartbeat expose le serveur HTTP qui reçoit les flux sortants des agents
// (POST /v1alpha1/heartbeat et POST /v1alpha1/logs, contrat §5).
// Il met à jour le status des McuNode K8s en conséquence.
package heartbeat

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/embewi/core/api/v1alpha1"
	"github.com/embewi/core/internal/metrics"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// wsUpgrader accepte les connexions WebSocket entrantes des agents ESP.
// CheckOrigin est permissif : les devices ESP n'envoient pas d'en-tête Origin.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsReadDeadline / wsPingInterval : sans deadline, un client qui upgrade puis ne
// parle plus jamais bloque ReadMessage() indéfiniment — fuite goroutine+socket
// atteignable avant même l'authentification (slow-loris). Le ping périodique
// maintient la deadline glissante tant que la connexion répond.
const (
	wsReadDeadline = 60 * time.Second
	wsPingInterval = 20 * time.Second
	wsPingTimeout  = 5 * time.Second
)

// validNodeStates reflète l'enum imposé par le CRD McuNode (spec.status.state) —
// une valeur hors de cette liste ferait échouer tout le Status().Patch (pas
// seulement le champ state), y compris l'IP et LastHeartbeat du même heartbeat.
var validNodeStates = map[string]bool{
	"booting":        true,
	"pending_verify": true,
	"running":        true,
	"degraded":       true,
	"rollback":       true,
	"failed":         true,
	"offline":        true,
}

// HeartbeatPayload correspond au corps POST /v1alpha1/heartbeat (contrat §5).
// Champs requis : node_id, ip, ts, state, ota_validated, config_generation.
type HeartbeatPayload struct {
	NodeID           string  `json:"node_id"`
	IP               string  `json:"ip"`
	TS               int64   `json:"ts"`
	State            string  `json:"state"`
	DeploymentID     string  `json:"deployment_id"`
	FirmwareDigest   string  `json:"firmware_digest"`
	OtaValidated     bool    `json:"ota_validated"`
	UptimeMs         int64   `json:"uptime_ms"`
	HeapFree         int     `json:"heap_free"`
	RSSI             int     `json:"rssi"`
	ConfigGeneration int     `json:"config_generation"`
	TempCelsius      float64 `json:"temp_celsius"`
	TaskHwmMin       int     `json:"task_hwm_min"`
	// Reason : canal de détresse dégradé (§5 contrat). "clock_unsynced" signale un
	// heartbeat émis avant que SNTP n'ait convergé, sur une connexion dont la
	// validité temporelle du certificat Core n'a pas pu être vérifiée côté agent —
	// signal diagnostique, jamais une confirmation opérationnelle complète (cf. ready).
	Reason string `json:"reason,omitempty"`
}

// reasonClockUnsynced : cf. HeartbeatPayload.Reason (§5 contrat, canal de détresse NTP).
const reasonClockUnsynced = "clock_unsynced"

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
	TLSCertFile string               // chemin PEM du certificat serveur (vide = HTTP plain)
	TLSKeyFile  string               // chemin PEM de la clé privée
	TokenSecret string               // nom du Secret K8s contenant les tokens Bearer (défaut : "embewi-tokens")
	Recorder    record.EventRecorder // émet HeartbeatIPMismatch (§1/§8 contrat) ; nil = détection désactivée
}

func New(addr string, c client.Client) *Server {
	return &Server{addr: addr, client: c}
}

// tokenMatch identifie quel token du Secret a authentifié la requête — nécessaire
// pour piloter la fenêtre de rotation sans coupure (§4 contrat) : un heartbeat
// authentifié avec l'ancien token n'est pas une anomalie tant que le device n'a
// pas confirmé le nouveau, et le premier heartbeat sur le nouveau token efface la
// rétention (cf. clearPreviousToken).
type tokenMatch int

const (
	tokenNoMatch tokenMatch = iota
	tokenMatchCurrent
	tokenMatchPrevious
)

// resolveTokenSecret retourne la clé du Secret K8s portant le(s) token(s) Bearer de
// ce device et la clé de donnée à lire pour le token courant. node.Spec.TokenRef
// prioritaire (clé de donnée "token", supporte la rotation via "previousToken") ;
// fallback sur un Secret centralisé (clé = nodeId, pas de support de rotation dans
// ce mode legacy de migration/test).
func (s *Server) resolveTokenSecret(node *v1alpha1.McuNode) (client.ObjectKey, string) {
	secretName := node.Spec.TokenRef.Name
	secretNS := node.Spec.TokenRef.Namespace
	if secretNS == "" {
		secretNS = node.Namespace
	}
	tokenDataKey := "token"
	if secretName == "" {
		// Fallback : Secret centralisé (clé = nodeId) — migration ou test.
		secretName = s.TokenSecret
		if secretName == "" {
			secretName = "embewi-tokens"
		}
		secretNS = node.Namespace
		tokenDataKey = node.Spec.NodeID
	}
	return client.ObjectKey{Name: secretName, Namespace: secretNS}, tokenDataKey
}

// validateToken vérifie le Bearer token du heartbeat contre le Secret K8s référencé
// par node.Spec.TokenRef (§1 contrat). Comparaison temps-constant. Accepte aussi
// data["previousToken"] pendant une fenêtre de rotation (§4 contrat) — retourne
// laquelle des deux clés a matché, et le Secret lu (pour clearPreviousToken).
func (s *Server) validateToken(ctx context.Context, r *http.Request, node *v1alpha1.McuNode) (tokenMatch, *corev1.Secret) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return tokenNoMatch, nil
	}
	provided := []byte(strings.TrimPrefix(auth, "Bearer "))

	secretKey, tokenDataKey := s.resolveTokenSecret(node)
	var secret corev1.Secret
	if err := s.client.Get(ctx, secretKey, &secret); err != nil {
		log.FromContext(ctx).Error(err, "lecture Secret token échouée", "secret", secretKey.Name)
		return tokenNoMatch, nil
	}

	if expected, ok := secret.Data[tokenDataKey]; ok &&
		subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(expected))), provided) == 1 {
		return tokenMatchCurrent, &secret
	}
	// previousToken uniquement supporté sur le Secret par node (pas le fallback centralisé).
	if tokenDataKey == "token" {
		if prev, ok := secret.Data["previousToken"]; ok && len(prev) > 0 &&
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(prev))), provided) == 1 {
			return tokenMatchPrevious, &secret
		}
	}
	return tokenNoMatch, nil
}

// clearPreviousToken efface McuSecret.previousToken une fois la rotation confirmée
// par un heartbeat authentifié avec le nouveau token (§4 contrat, rétention).
func (s *Server) clearPreviousToken(ctx context.Context, secret *corev1.Secret) {
	patch := client.MergeFrom(secret.DeepCopy())
	delete(secret.Data, "previousToken")
	if err := s.client.Patch(ctx, secret, patch); err != nil {
		log.FromContext(ctx).Error(err, "effacement previousToken échoué", "secret", secret.Name)
	}
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
		if errors.Is(err, errDuplicateNodeID) {
			logger.Error(err, "node_id dupliqué — heartbeat ignoré tant que le doublon n'est pas résolu")
		} else {
			logger.Error(err, "McuNode introuvable pour ce node_id")
		}
		// On répond 200 : l'agent ne doit pas crasher si le Core ne connaît pas encore ce node
		// (ou si le node_id est ambigu — même logique fail-safe côté device).
		w.WriteHeader(http.StatusOK)
		return
	}

	// Validation Bearer token — comparaison temps-constant (§1 contrat). Accepte
	// aussi previousToken pendant une fenêtre de rotation (§4) : ce n'est pas une
	// anomalie, c'est le signal attendu tant que le device n'a pas confirmé le
	// nouveau token.
	match, secret := s.validateToken(r.Context(), r, node)
	if match == tokenNoMatch {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Premier heartbeat authentifié avec le nouveau token → rotation confirmée,
	// previousToken n'a plus d'utilité (§4 contrat).
	if match == tokenMatchCurrent && len(secret.Data["previousToken"]) > 0 {
		s.clearPreviousToken(r.Context(), secret)
	}

	// state hors enum CRD → tout le Status().Patch échouerait (pas seulement ce champ).
	if !validNodeStates[hb.State] {
		logger.Error(nil, "heartbeat : state hors enum CRD, ignoré")
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	// IP du device : contrat §5/§8 — utiliser heartbeat.ip, pas RemoteAddr.
	// Fallback sur RemoteAddr si le champ est absent (compatibilité).
	tcpIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	sourceIP := hb.IP
	if sourceIP == "" {
		sourceIP = tcpIP
	}

	// Détection de divergence heartbeat.ip / IP source TCP (§1 rayon d'action d'un
	// token compromis, §8 contrat) : un token volé permettrait de déclarer une ip
	// arbitraire et détourner l'EndpointSlice. Détection seule — un proxy ou un
	// routage asymétrique légitime peut aussi produire cette divergence, donc on
	// ne rejette ni ne bloque la mise à jour de l'EndpointSlice.
	if s.Recorder != nil && hb.IP != "" && tcpIP != "" && hb.IP != tcpIP {
		s.Recorder.Eventf(node, corev1.EventTypeWarning, "HeartbeatIPMismatch",
			"heartbeat.ip=%s diffère de l'IP source TCP=%s", hb.IP, tcpIP)
	}

	now := metav1.NewTime(time.Now())
	clockUnsynced := hb.Reason == reasonClockUnsynced
	ready := hb.State == "running" && hb.OtaValidated && !clockUnsynced

	// Verrou optimiste + retry : le reconciler McuNode (mcunode_controller.go) patch
	// le même Status de façon concurrente (timeout heartbeat toutes les 10s). Sans
	// ça, un reconcile basé sur une lecture pas encore rafraîchie pourrait écraser
	// ce heartbeat tout juste écrit. Re-Get à chaque tentative pour repartir d'une
	// base fraîche plutôt que de réappliquer un diff périmé.
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := s.client.Get(r.Context(), client.ObjectKeyFromObject(node), node); err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
		node.Status.IP = sourceIP
		node.Status.State = hb.State
		node.Status.FirmwareDigest = hb.FirmwareDigest
		node.Status.DeploymentID = hb.DeploymentID
		node.Status.OtaValidated = hb.OtaValidated
		node.Status.HeapFree = hb.HeapFree
		node.Status.RSSI = hb.RSSI
		node.Status.UptimeMs = hb.UptimeMs
		node.Status.ConfigGeneration = hb.ConfigGeneration
		node.Status.TaskHwmMin = hb.TaskHwmMin
		node.Status.Ready = ready
		node.Status.LastHeartbeat = &now

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
		switch {
		case ready:
			apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "HeartbeatOK",
				Message: fmt.Sprintf("heartbeat reçu, state=%s", hb.State),
			})
		case clockUnsynced:
			// Canal de détresse NTP (§5 contrat) : signal diagnostique distinct de
			// HeartbeatTimeout (device vivant, mais confirmation non fiable tant que
			// l'horloge n'est pas posée) — jamais promu en Ready=True, même si
			// state=running et ota_validated=true par ailleurs.
			apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionFalse,
				Reason:  "ClockUnsynced",
				Message: "heartbeat reçu mais horloge device non synchronisée (NTP) — signal diagnostique, pas de confirmation Ready",
			})
		default:
			// Heartbeat reçu mais device pas encore prêt (pending_verify, degraded…).
			// Raison distincte de HeartbeatTimeout pour différencier device vivant vs silencieux.
			apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionFalse,
				Reason:  "DeviceNotReady",
				Message: fmt.Sprintf("heartbeat reçu, state=%s (non prêt)", hb.State),
			})
		}

		return s.client.Status().Patch(r.Context(), node, patch)
	})
	if err != nil {
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
	// WebSocket upgrade : l'agent ouvre une connexion WS cliente en sortant (contrat §5).
	if websocket.IsWebSocketUpgrade(r) {
		s.handleLogWS(w, r)
		return
	}
	// HTTP POST : fallback pour les événements OTA/lifecycle critiques.
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2048))

	var entry LogPayload
	if err := json.Unmarshal(body, &entry); err != nil {
		w.WriteHeader(http.StatusOK) // absorbe les entrées malformées
		return
	}
	s.logEntry(r.Context(), entry)
	w.WriteHeader(http.StatusOK)
}

// handleLogWS gère le flux de logs WebSocket émis par l'agent (contrat §5).
// L'agent ouvre la connexion en sortant (outbound) — le Core est serveur.
// Auth différée sur le premier frame : le champ "node" identifie le device,
// le Bearer header est alors validé via le Secret référencé dans McuNodeSpec.TokenRef.
// Garantie best-effort : pas de replay sur reconnexion.
func (s *Server) handleLogWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade échoué — réponse HTTP déjà envoyée par gorilla.
		return
	}
	defer conn.Close()

	ctx := r.Context()
	logger := log.FromContext(ctx)
	authenticated := false

	_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
	})

	// Ping périodique dans une goroutine dédiée : WriteControl/Close sont les
	// seules méthodes gorilla documentées comme sûres en concurrence avec la
	// boucle de lecture ci-dessous.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsPingTimeout)); err != nil {
					_ = conn.Close() // débloque le ReadMessage() de la boucle principale
					return
				}
			case <-done:
				return
			}
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// Déconnexion normale (EOF, close frame) ou deadline expirée — best-effort, pas d'erreur.
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) &&
				!websocket.IsUnexpectedCloseError(err) {
				logger.Error(err, "WS logs : lecture frame échouée")
			}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))

		var entry LogPayload
		if json.Unmarshal(raw, &entry) != nil {
			continue // frame malformée — best-effort
		}

		// Auth différée sur le premier frame : node_id connu → validation Bearer.
		if !authenticated {
			node, err := s.findNode(ctx, entry.Node)
			if err != nil {
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "unauthorized"))
				return
			}
			if match, _ := s.validateToken(ctx, r, node); match == tokenNoMatch {
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "unauthorized"))
				return
			}
			authenticated = true
			logger.Info("WS logs : connexion authentifiée", "node", entry.Node)
		}

		s.logEntry(ctx, entry)
	}
}

// logEntry écrit un LogPayload dans le logger structuré.
func (s *Server) logEntry(ctx context.Context, entry LogPayload) {
	logger := log.FromContext(ctx)
	switch entry.Level {
	case "fatal", "error":
		logger.Error(nil, entry.Msg, "node", entry.Node, "workload", entry.Workload)
	default:
		logger.Info(entry.Msg, "node", entry.Node, "workload", entry.Workload, "level", entry.Level)
	}
}

// errDuplicateNodeID signale que plusieurs McuNode déclarent le même spec.nodeId
// (§1a contrat : « le Core est responsable de refuser un node_id en double »).
var errDuplicateNodeID = errors.New("node_id dupliqué entre plusieurs McuNode")

// findNode cherche un McuNode dont spec.nodeId == nodeID dans tous les namespaces.
// Si plusieurs McuNode matchent, il n'existe aucun moyen sûr de choisir lequel
// représente le device physique : plutôt que d'attacher silencieusement le
// heartbeat au premier trouvé (et de corrompre son status), on refuse et on
// signale le doublon par Event sur chaque objet concerné.
func (s *Server) findNode(ctx context.Context, nodeID string) (*v1alpha1.McuNode, error) {
	var list v1alpha1.McuNodeList
	if err := s.client.List(ctx, &list); err != nil {
		return nil, err
	}
	var matches []*v1alpha1.McuNode
	for i := range list.Items {
		if list.Items[i].Spec.NodeID == nodeID {
			matches = append(matches, &list.Items[i])
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no McuNode with nodeId=%q", nodeID)
	case 1:
		return matches[0], nil
	default:
		if s.Recorder != nil {
			for _, m := range matches {
				s.Recorder.Eventf(m, corev1.EventTypeWarning, "DuplicateNodeID",
					"%d McuNode déclarent nodeId=%q — heartbeats de ce device ignorés jusqu'à résolution", len(matches), nodeID)
			}
		}
		return nil, fmt.Errorf("%w: %d McuNode déclarent nodeId=%q", errDuplicateNodeID, len(matches), nodeID)
	}
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
