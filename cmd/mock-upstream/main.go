// mock-upstream is a controllable, local HTTP server for manual proxy checks.
// Its control API is deliberately separate from the upstream listener.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type scriptedEvent struct {
	DelayMS int             `json:"delay_ms,omitempty"`
	Data    json.RawMessage `json:"data"`
}

type scriptedResponse struct {
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	DelayMS int               `json:"delay_ms,omitempty"`
	Events  []scriptedEvent   `json:"events,omitempty"`
}

type requestRecord struct {
	At                    time.Time `json:"at"`
	FinishedAt            time.Time `json:"finished_at,omitempty"`
	Method                string    `json:"method"`
	Path                  string    `json:"path"`
	AuthorizationPresent  bool      `json:"authorization_present"`
	AuthorizationScheme   string    `json:"authorization_scheme,omitempty"`
	AuthorizationRedacted bool      `json:"authorization_redacted"`
	AuthorizationChecked  bool      `json:"authorization_checked"`
	AuthorizationMatches  bool      `json:"authorization_matches_expected"`
	Body                  string    `json:"body,omitempty"`
	Status                int       `json:"status"`
	Canceled              bool      `json:"canceled,omitempty"`
}

type mockServer struct {
	mu             sync.Mutex
	queues         map[string][]scriptedResponse
	requests       []requestRecord
	counts         map[string]int
	nextID         int64
	expectedBearer string
}

// newMockServer starts with isolated control state and a predictable background completion delay.
func newMockServer() *mockServer {
	return &mockServer{
		queues: make(map[string][]scriptedResponse),
		counts: make(map[string]int),
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:19090", "upstream HTTP listener")
	admin := flag.String("admin-listen", "127.0.0.1:19091", "local control API listener")
	expectedBearer := flag.String("expected-bearer", "", "optional synthetic upstream key to verify without recording it")
	cert := flag.String("tls-cert", "", "TLS certificate PEM (enables HTTPS when set)")
	key := flag.String("tls-key", "", "TLS private key PEM")
	genPKI := flag.String("generate-test-pki", "", "write a local test CA and localhost server certificate into this directory, then exit")
	flag.Parse()

	if *genPKI != "" {
		if err := generateTestPKI(*genPKI); err != nil {
			log.Fatal(err)
		}
		return
	}
	// Keep the control API local because it exposes synthetic request bodies and can alter queued responses.
	if (*cert == "") != (*key == "") {
		log.Fatal("both --tls-cert and --tls-key are required")
	}
	if !isLoopbackAddress(*admin) {
		log.Fatal("the control API listener must use a loopback address")
	}

	mock := newMockServer()
	mock.expectedBearer = *expectedBearer
	upstreamMux := http.NewServeMux()
	upstreamMux.HandleFunc("/", mock.serveUpstream)
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/__control/queue", mock.controlQueue)
	adminMux.HandleFunc("/__control/state", mock.controlState)
	adminMux.HandleFunc("/__control/reset", mock.controlReset)

	upstreamServer := &http.Server{Addr: *listen, Handler: upstreamMux, ReadHeaderTimeout: 5 * time.Second}
	adminServer := &http.Server{Addr: *admin, Handler: adminMux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("mock control API listening on http://%s", *admin)
		if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	// TLS is optional for upstream scenarios; the separate control listener remains plain loopback HTTP.
	if *cert != "" {
		log.Printf("mock upstream listening on https://%s", *listen)
		log.Fatal(upstreamServer.ListenAndServeTLS(*cert, *key))
	}
	log.Printf("mock upstream listening on http://%s", *listen)
	log.Fatal(upstreamServer.ListenAndServe())
}

// serveUpstream records requests with Authorization values redacted, then applies a queued or default response.
func (m *mockServer) serveUpstream(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	key := r.Method + " " + r.URL.Path
	parts := strings.Fields(r.Header.Get("Authorization"))
	// Record the URI for forwarding checks, but retain only the authorization scheme and a synthetic-key match.
	record := requestRecord{
		At: time.Now().UTC(), Method: r.Method, Path: r.URL.RequestURI(),
		AuthorizationPresent: len(parts) > 0, AuthorizationRedacted: true,
		Body: string(body),
	}
	if len(parts) > 0 {
		record.AuthorizationScheme = parts[0]
	}
	m.mu.Lock()
	if m.expectedBearer != "" {
		record.AuthorizationChecked = true
		record.AuthorizationMatches = r.Header.Get("Authorization") == "Bearer "+m.expectedBearer
	}
	m.counts[key]++
	m.requests = append(m.requests, record)
	requestIndex := len(m.requests) - 1
	// Consume a scripted response while holding the lock so concurrent requests cannot reuse it.
	queue := m.queues[key]
	var scripted *scriptedResponse
	if len(queue) > 0 {
		copyOfFirst := queue[0]
		scripted = &copyOfFirst
		m.queues[key] = queue[1:]
	}
	m.mu.Unlock()

	if scripted == nil {
		m.serveDefault(w, r, body, requestIndex)
		return
	}
	if scripted.DelayMS > 0 {
		timer := time.NewTimer(time.Duration(scripted.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			m.markCanceled(requestIndex)
			return
		case <-timer.C:
		}
	}
	// Apply scripted headers only after the initial delay, letting cancellation finish without a response.
	for name, value := range scripted.Headers {
		w.Header().Set(name, value)
	}
	status := scripted.Status
	if status == 0 {
		status = http.StatusOK
	}
	if len(scripted.Events) > 0 {
		m.serveSSE(w, r, status, scripted.Events, requestIndex)
		return
	}
	// A body-less scripted reply is still useful for status-only error scenarios.
	if len(scripted.Body) == 0 {
		scripted.Body = json.RawMessage(`{"error":{"message":"mock response"}}`)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(scripted.Body)
	_, _ = io.WriteString(w, "\n")
	m.markStatus(requestIndex, status)
}

// serveDefault implements POST /v1/responses for manual proxy scenarios.
func (m *mockServer) serveDefault(w http.ResponseWriter, r *http.Request, requestBody []byte, requestIndex int) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/responses":
		var request map[string]any
		_ = json.Unmarshal(requestBody, &request)
		// Allocate IDs under the mutex so parallel creates cannot collide.
		m.mu.Lock()
		m.nextID++
		id := fmt.Sprintf("resp_mock_%06d", m.nextID)
		m.mu.Unlock()
		model, _ := request["model"].(string)
		if model == "" {
			model = "gpt-6-luna"
		}
		resp := map[string]any{
			"id": id, "object": "response", "created_at": time.Now().Unix(),
			"status": "completed", "model": model, "output": []any{}, "error": nil,
			"usage": map[string]any{
				"input_tokens": 12, "input_tokens_details": map[string]any{"cached_tokens": 2, "cache_write_tokens": 1},
				"output_tokens": 4, "output_tokens_details": map[string]any{"reasoning_tokens": 1},
				"total_tokens": 16,
			},
		}
		if request["background"] == true {
			resp["status"] = "queued"
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") || request["stream"] == true {
			m.serveSSE(w, r, http.StatusOK, []scriptedEvent{{Data: jsonValue(map[string]any{"type": "response.completed", "response": resp})}}, requestIndex)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		m.markStatus(requestIndex, http.StatusOK)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "mock route not found"}})
		m.markStatus(requestIndex, http.StatusNotFound)
	}
}

// serveSSE writes complete events and honors cancellation during scripted delays.
func (m *mockServer) serveSSE(w http.ResponseWriter, r *http.Request, status int, events []scriptedEvent, requestIndex int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		if event.DelayMS > 0 {
			timer := time.NewTimer(time.Duration(event.DelayMS) * time.Millisecond)
			select {
			case <-r.Context().Done():
				timer.Stop()
				m.markCanceled(requestIndex)
				return
			case <-timer.C:
			}
		}
		if len(event.Data) == 0 {
			continue
		}
		// Each scripted payload is one complete SSE event, matching the proxy's blank-line framing.
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event.Data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Finish with the standard stream sentinel after all queued events.
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	m.markStatus(requestIndex, status)
}

func (m *mockServer) controlQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Path      string             `json:"path"`
		Method    string             `json:"method"`
		Responses []scriptedResponse `json:"responses"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&request); err != nil || request.Path == "" {
		http.Error(w, "expected path and responses", http.StatusBadRequest)
		return
	}
	if request.Method == "" {
		request.Method = http.MethodPost
	}
	// Queue by method and path so scenarios can target one upstream route precisely.
	key := request.Method + " " + request.Path
	m.mu.Lock()
	m.queues[key] = append(m.queues[key], request.Responses...)
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"queued": len(request.Responses), "key": key})
}

func (m *mockServer) controlState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Return copies so callers see a stable snapshot without receiving mutable internal maps or slices.
	requests := append([]requestRecord(nil), m.requests...)
	counts := make(map[string]int, len(m.counts))
	for key, value := range m.counts {
		counts[key] = value
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts, "requests": requests})
}

func (m *mockServer) controlReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	m.mu.Lock()
	// Clear scripted scenarios and observed requests without resetting response IDs.
	m.queues = make(map[string][]scriptedResponse)
	m.requests = nil
	m.counts = make(map[string]int)
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

func (m *mockServer) markStatus(index, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Handler completion updates the original record rather than adding another request entry.
	if index >= 0 && index < len(m.requests) {
		m.requests[index].Status = status
		m.requests[index].FinishedAt = time.Now().UTC()
	}
}

func (m *mockServer) markCanceled(index int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index >= 0 && index < len(m.requests) {
		m.requests[index].Canceled = true
		m.requests[index].FinishedAt = time.Now().UTC()
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func jsonValue(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func generateTestPKI(dir string) error {
	// Generate a short-lived test CA and localhost certificate for local TLS checks.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	now := time.Now().UTC()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	caTemplate := &x509.Certificate{
		SerialNumber: serialNumber(), Subject: pkix.Name{CommonName: "Responses Proxy Manual Test CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serialNumber(), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		// Restrict hostname verification to the local test name rather than granting arbitrary hostnames.
		DNSNames:    []string{"localhost"},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"ca.pem":         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"server.pem":     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		"server-key.pem": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)}),
		"ca-key.pem":     pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)}),
	}
	for name, data := range files {
		mode := os.FileMode(0o644)
		// Keep private key material readable only by the local test user.
		if strings.Contains(name, "key") {
			mode = 0o600
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
			return err
		}
	}
	return nil
}

func serialNumber() *big.Int {
	// Use random serials, with a time-based fallback if entropy fails.
	limit := new(big.Int).Lsh(big.NewInt(1), 120)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	// Accept only localhost or a literal loopback IP, avoiding hostnames that may resolve remotely.
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
