package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"golang.org/x/oauth2"
)

//go:embed static/index.html
var indexHTML []byte

type config struct {
	dataDir     string
	natsURL     string
	monitorURL  string
	apiURL      string
	logoutURL   string
	publicURL   string
	oauth       *oauth2.Config
	publishEach time.Duration
}

type incoming struct {
	At      string `json:"at"`
	Subject string `json:"subject"`
	Payload string `json:"payload"`
}

type backend struct {
	cfg config

	mu            sync.Mutex
	dev           Device
	sec           Secrets
	op            string // in-progress operation shown in the UI ("" when idle)
	lastErr       string
	token         *oauth2.Token
	user          string
	natsUp        bool
	natsDowns     int // times the nats connection dropped
	leafUp        bool
	published     int
	lastPublished string
	recent        []incoming
	pending       map[string]string // oauth state -> pkce verifier
}

func runServe(dataDir string) {
	b := &backend{pending: map[string]string{}}
	b.cfg = config{
		dataDir:     dataDir,
		natsURL:     mustEnv("NATS_URL"),
		monitorURL:  mustEnv("NATS_MONITOR_URL"),
		apiURL:      mustEnv("API_URL"),
		publicURL:   mustEnv("PUBLIC_URL"),
		logoutURL:   mustEnv("OIDC_LOGOUT_URL"),
		publishEach: 3 * time.Second,
		oauth: &oauth2.Config{
			ClientID: env("OIDC_CLIENT_ID", "local-device"),
			Endpoint: oauth2.Endpoint{
				AuthURL:   mustEnv("OIDC_AUTH_URL"),  // browser-facing
				TokenURL:  mustEnv("OIDC_TOKEN_URL"), // container-to-container
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: strings.TrimRight(mustEnv("PUBLIC_URL"), "/") + "/auth/callback",
			Scopes:      []string{"openid"},
		},
	}
	if err := readJSON(devicePath(dataDir), &b.dev); err != nil {
		log.Fatalf("read device.json (did init run?): %v", err)
	}
	if err := readJSON(secretsPath(dataDir), &b.sec); err != nil {
		log.Fatalf("read secrets.json: %v", err)
	}
	log.Printf("device %s (%s) registered=%v", b.dev.ID, b.dev.Name, b.dev.Registered)

	go b.publishLoop()
	go b.monitorLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/status", b.handleStatus)
	mux.HandleFunc("GET /auth/login", b.handleLogin)
	mux.HandleFunc("GET /auth/callback", b.handleCallback)
	mux.HandleFunc("POST /auth/logout", b.ui(b.handleLogout))
	mux.HandleFunc("POST /api/register", b.ui(b.handleRegister))
	mux.HandleFunc("POST /api/unregister", b.ui(b.handleUnregister))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	addr := env("LISTEN", ":8080")
	log.Printf("local-backend listening on %s (public %s)", addr, b.cfg.publicURL)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s must be set", k)
	}
	return v
}

// ui guards state-changing endpoints against cross-site form posts.
func (b *backend) ui(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Requested-With") != "ui" {
			jsonErr(w, 403, "forbidden")
			return
		}
		h(w, r)
	}
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}

// ---- data publishing ------------------------------------------------------

// publishLoop publishes device data to LOCAL nats the whole time, registered
// or not. Before registration (no leafnode) core NATS drops it; after, it
// flows to the remote. A NATS restart is ridden out by client reconnect; the
// small reconnect buffer keeps a few seconds of in-flight data at most.
func (b *backend) publishLoop() {
	b.mu.Lock()
	id, name := b.dev.ID, b.dev.Name
	user, pass := b.sec.NatsUser, b.sec.NatsPass
	b.mu.Unlock()

	nc, err := nats.Connect(b.cfg.natsURL,
		nats.UserInfo(user, pass),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.ReconnectBufSize(32*1024),
		nats.DisconnectErrHandler(func(*nats.Conn, error) { b.setNatsUp(false) }),
		nats.ConnectHandler(func(*nats.Conn) { b.setNatsUp(true) }),
		nats.ReconnectHandler(func(*nats.Conn) { b.setNatsUp(true); log.Println("reconnected to local nats") }),
	)
	if err != nil {
		log.Fatalf("nats connect: %v", err)
	}
	if nc.IsConnected() {
		b.setNatsUp(true)
	}

	// Subscriptions are re-established automatically on reconnect.
	incomingSubj := fmt.Sprintf("device.%s.incoming.data", id)
	if _, err := nc.Subscribe(incomingSubj, func(m *nats.Msg) {
		b.mu.Lock()
		b.recent = append([]incoming{{At: time.Now().Format("15:04:05"), Subject: m.Subject, Payload: string(m.Data)}}, b.recent...)
		if len(b.recent) > 8 {
			b.recent = b.recent[:8]
		}
		b.mu.Unlock()
	}); err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	outSubj := fmt.Sprintf("device.%s.outgoing.data", id)
	for range time.Tick(b.cfg.publishEach) {
		payload := fmt.Sprintf(`{"device_id":"%s","name":"%s","value":%d}`, id, name, rand.Intn(1000))
		if err := nc.Publish(outSubj, []byte(payload)); err != nil {
			log.Printf("publish failed: %v", err)
			continue
		}
		b.mu.Lock()
		b.published++
		b.lastPublished = time.Now().Format("15:04:05")
		b.mu.Unlock()
	}
}

func (b *backend) setNatsUp(up bool) {
	b.mu.Lock()
	if !up && b.natsUp {
		b.natsDowns++
	}
	b.natsUp = up
	b.mu.Unlock()
}

// monitorLoop polls the local NATS monitoring endpoint for leafnode state.
func (b *backend) monitorLoop() {
	client := &http.Client{Timeout: 2 * time.Second}
	for range time.Tick(time.Second) {
		up := false
		resp, err := client.Get(b.cfg.monitorURL + "/leafz")
		if err == nil {
			var lz struct {
				Leafs []json.RawMessage `json:"leafs"`
			}
			if json.NewDecoder(resp.Body).Decode(&lz) == nil {
				up = len(lz.Leafs) > 0
			}
			resp.Body.Close()
		}
		b.mu.Lock()
		b.leafUp = up
		b.mu.Unlock()
	}
}

// ---- status ---------------------------------------------------------------

func (b *backend) state() string {
	switch {
	case b.op != "":
		return b.op
	case !b.dev.Registered:
		return "unregistered"
	case b.leafUp:
		return "connected"
	default:
		return "disconnected"
	}
}

func (b *backend) handleStatus(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	jsonOut(w, 200, map[string]any{
		"device_id":      b.dev.ID,
		"name":           b.dev.Name,
		"state":          b.state(),
		"registered":     b.dev.Registered,
		"error":          b.lastErr,
		"logged_in":      b.token != nil,
		"user":           b.user,
		"nats_up":        b.natsUp,
		"leaf_up":        b.leafUp,
		"published":      b.published,
		"last_published": b.lastPublished,
		"recent":         b.recent,
	})
}

// ---- login (Authorization Code + PKCE) -------------------------------------

func (b *backend) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, verifier := randHex(16), oauth2.GenerateVerifier()
	b.mu.Lock()
	b.pending[state] = verifier
	b.mu.Unlock()
	http.Redirect(w, r, b.cfg.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (b *backend) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	b.mu.Lock()
	verifier, ok := b.pending[state]
	delete(b.pending, state)
	b.mu.Unlock()
	if !ok {
		http.Error(w, "unknown or expired login state", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "login failed: "+e, http.StatusBadRequest)
		return
	}
	tok, err := b.cfg.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		log.Printf("token exchange: %v", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	b.mu.Lock()
	b.token = tok
	b.user = usernameFromToken(tok.AccessToken)
	b.lastErr = ""
	b.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusFound)
}

// usernameFromToken reads preferred_username for display only; the token is
// validated by registration-api, never trusted here.
func usernameFromToken(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		U string `json:"preferred_username"`
	}
	json.Unmarshal(raw, &c)
	return c.U
}

func (b *backend) handleLogout(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.token, b.user = nil, ""
	b.mu.Unlock()
	u := b.cfg.logoutURL + "?" + url.Values{
		"client_id":                {b.cfg.oauth.ClientID},
		"post_logout_redirect_uri": {strings.TrimRight(b.cfg.publicURL, "/") + "/"},
	}.Encode()
	jsonOut(w, 200, map[string]string{"redirect": u})
}

// accessToken returns a valid access token (refreshing if needed).
func (b *backend) accessToken(ctx context.Context) (string, error) {
	b.mu.Lock()
	tok := b.token
	b.mu.Unlock()
	if tok == nil {
		return "", errors.New("not logged in")
	}
	fresh, err := b.cfg.oauth.TokenSource(ctx, tok).Token()
	if err != nil {
		b.mu.Lock()
		b.token, b.user = nil, ""
		b.mu.Unlock()
		return "", errors.New("session expired, log in again")
	}
	b.mu.Lock()
	b.token = fresh
	b.mu.Unlock()
	return fresh.AccessToken, nil
}

// ---- register / unregister --------------------------------------------------

// beginOp marks an operation in progress, rejecting concurrent ones.
func (b *backend) beginOp(op string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.op != "" {
		return false
	}
	b.op, b.lastErr = op, ""
	return true
}

func (b *backend) setOp(op string) { b.mu.Lock(); b.op = op; b.mu.Unlock() }

func (b *backend) endOp(err error) {
	b.mu.Lock()
	b.op = ""
	if err != nil {
		b.lastErr = err.Error()
		log.Printf("operation failed: %v", err)
	}
	b.mu.Unlock()
}

func (b *backend) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)

	tok, err := b.accessToken(r.Context())
	if err != nil {
		jsonErr(w, 401, err.Error())
		return
	}
	if !b.beginOp("registering") {
		jsonErr(w, 409, "another operation is in progress")
		return
	}
	b.mu.Lock()
	id, name := b.dev.ID, b.dev.Name
	b.mu.Unlock()
	if n := strings.TrimSpace(req.Name); n != "" {
		name = n
	}
	go func() { b.endOp(b.register(tok, id, name)) }()
	jsonOut(w, 202, map[string]string{"status": "registering"})
}

func (b *backend) register(token, id, name string) error {
	body, _ := json.Marshal(map[string]string{"device_id": id, "name": name})
	req, _ := http.NewRequest("POST", b.cfg.apiURL+"/v1/devices/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("registration-api unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("registration failed: %s", apiError(resp.StatusCode, raw))
	}
	var out struct {
		LeafURL string `json:"leaf_url"`
		Creds   string `json:"creds"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.LeafURL == "" || out.Creds == "" {
		return errors.New("registration-api returned an invalid response")
	}

	nd := natsDir(b.cfg.dataDir)
	credsPath, leafPath := filepath.Join(nd, "leaf.creds"), filepath.Join(nd, "leaf.conf")
	prevCreds, _ := os.ReadFile(credsPath)
	prevLeaf, _ := os.ReadFile(leafPath)

	newLeaf := fmt.Sprintf("leafnodes {\n  remotes = [\n    { url: %q, credentials: \"/nats/leaf.creds\" }\n  ]\n}\n", out.LeafURL)
	if err := writeFileAtomic(credsPath, []byte(out.Creds), 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(leafPath, []byte(newLeaf), 0o644); err != nil {
		return err
	}

	b.setOp("restarting nats")
	if err := b.restartNats(); err != nil {
		b.rollbackLeaf(credsPath, leafPath, prevCreds, prevLeaf)
		return err
	}
	b.setOp("connecting to remote")
	if !b.waitFor(func() bool { return b.leafUp }, 30*time.Second) {
		b.rollbackLeaf(credsPath, leafPath, prevCreds, prevLeaf)
		return errors.New("leafnode did not connect to the remote within 30s; previous config restored")
	}

	b.mu.Lock()
	b.dev.Name, b.dev.Registered, b.dev.RegisteredAt = name, true, time.Now().UTC().Format(time.RFC3339)
	dev := b.dev
	b.mu.Unlock()
	if err := writeJSON(devicePath(b.cfg.dataDir), dev); err != nil {
		return err
	}
	log.Printf("registered with remote as %s", name)
	return nil
}

func (b *backend) rollbackLeaf(credsPath, leafPath string, prevCreds, prevLeaf []byte) {
	log.Println("rolling back leaf config")
	if len(prevCreds) > 0 {
		writeFileAtomic(credsPath, prevCreds, 0o600)
	} else {
		os.Remove(credsPath)
	}
	writeFileAtomic(leafPath, prevLeaf, 0o644)
	b.setOp("rolling back")
	if err := b.restartNats(); err != nil {
		log.Printf("rollback restart: %v", err)
	}
}

func (b *backend) handleUnregister(w http.ResponseWriter, r *http.Request) {
	tok, err := b.accessToken(r.Context())
	if err != nil {
		jsonErr(w, 401, "log in to revoke this device on the remote: "+err.Error())
		return
	}
	if !b.beginOp("unregistering") {
		jsonErr(w, 409, "another operation is in progress")
		return
	}
	go func() { b.endOp(b.unregister(tok)) }()
	jsonOut(w, 202, map[string]string{"status": "unregistering"})
}

func (b *backend) unregister(token string) error {
	b.mu.Lock()
	id := b.dev.ID
	b.mu.Unlock()

	req, _ := http.NewRequest("DELETE", b.cfg.apiURL+"/v1/devices/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("registration-api unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 && resp.StatusCode != 404 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("revoke failed: %s", apiError(resp.StatusCode, raw))
	}

	nd := natsDir(b.cfg.dataDir)
	if err := writeFileAtomic(filepath.Join(nd, "leaf.conf"), nil, 0o644); err != nil {
		return err
	}
	os.Remove(filepath.Join(nd, "leaf.creds"))
	b.setOp("restarting nats")
	if err := b.restartNats(); err != nil {
		return err
	}
	b.mu.Lock()
	b.dev.Registered, b.dev.RegisteredAt = false, ""
	dev := b.dev
	b.mu.Unlock()
	return writeJSON(devicePath(b.cfg.dataDir), dev)
}

func apiError(code int, raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Error, code)
	}
	return fmt.Sprintf("HTTP %d", code)
}

// ---- nats restart ------------------------------------------------------------

// restartNats asks nats-local's supervisor (run.sh) to restart nats-server by
// dropping a marker file, then waits for the server to go down and come back.
// The publisher's client reconnects on its own.
func (b *backend) restartNats() error {
	b.mu.Lock()
	downs := b.natsDowns
	b.mu.Unlock()
	if err := os.WriteFile(filepath.Join(natsDir(b.cfg.dataDir), "restart"), nil, 0o644); err != nil {
		return err
	}
	if !b.waitFor(func() bool { return b.natsDowns > downs }, 10*time.Second) {
		return errors.New("nats-local did not restart (is its supervisor running?)")
	}
	if !b.waitFor(func() bool { return b.natsUp }, 30*time.Second) {
		return errors.New("nats-local did not come back after restart")
	}
	return nil
}

func (b *backend) waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		ok := cond()
		b.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}
