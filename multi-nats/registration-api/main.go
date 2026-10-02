// registration-api lets an authenticated (Keycloak) user register a local
// device to the remote NATS: it validates the OIDC access token, mints a NATS
// user scoped to that device's own subjects, records it in a SQLite registry
// and returns the leafnode URL + creds. It also revokes devices.
package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	_ "modernc.org/sqlite"
)

var (
	uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	nameRe = regexp.MustCompile(`^[A-Za-z0-9 _.-]{1,64}$`)
)

type server struct {
	db         *sql.DB
	trust      *Trust
	verifier   *oidc.IDTokenVerifier
	clientID   string
	leafURL    string
	maxPerUser int
	staticTok  string // dev/test only: if set, "Bearer <staticTok>" authenticates as owner "static"
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	ctx := context.Background()

	db, err := sql.Open("sqlite", env("DB_PATH", "/data/registry.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS devices (
		device_id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		owner_sub TEXT NOT NULL,
		owner_name TEXT NOT NULL DEFAULT '',
		user_pub TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		last_registered_at TEXT NOT NULL,
		revoked_at TEXT
	)`); err != nil {
		log.Fatal(err)
	}

	trust, err := LoadTrust(env("STORE_DIR", "/nsc-data/store"), "DEVICES",
		env("SYS_CREDS", "/nsc-data/creds/sys.creds"), env("NATS_URL", "nats://nats-remote:4222"))
	if err != nil {
		log.Fatalf("load trust: %v", err)
	}

	s := &server{
		db:         db,
		trust:      trust,
		clientID:   env("OIDC_CLIENT_ID", "local-device"),
		leafURL:    env("LEAF_URL", "leaf://nats-remote:7422"),
		maxPerUser: atoi(env("MAX_DEVICES_PER_USER", "10")),
		staticTok:  os.Getenv("STATIC_TOKEN"),
	}

	// Keys are fetched from the internal Keycloak URL, but tokens carry the
	// external issuer URL the browser used.
	keys := oidc.NewRemoteKeySet(ctx, mustEnv("OIDC_JWKS_URL"))
	s.verifier = oidc.NewVerifier(mustEnv("OIDC_ISSUER"), keys, &oidc.Config{SkipClientIDCheck: true})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /v1/devices/register", s.auth(s.register))
	mux.HandleFunc("GET /v1/devices", s.auth(s.list))
	mux.HandleFunc("GET /v1/devices/{id}", s.auth(s.get))
	mux.HandleFunc("DELETE /v1/devices/{id}", s.auth(s.deregister))

	addr := env("LISTEN", ":8090")
	log.Printf("registration-api listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, logRequests(mux)))
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s must be set", k)
	}
	return v
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

type ctxKey struct{}

type owner struct{ Sub, Name string }

// auth validates the Bearer token (signature, issuer, expiry, and that it
// was issued to our client via azp) and passes the owner to the handler.
func (s *server) auth(h func(http.ResponseWriter, *http.Request, owner)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeErr(w, 401, "missing bearer token")
			return
		}
		if s.staticTok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.staticTok)) == 1 {
			h(w, r, owner{Sub: "static", Name: "static"})
			return
		}
		idt, err := s.verifier.Verify(r.Context(), tok)
		if err != nil {
			writeErr(w, 401, "invalid token")
			return
		}
		var c struct {
			Azp               string `json:"azp"`
			PreferredUsername string `json:"preferred_username"`
		}
		if err := idt.Claims(&c); err != nil || c.Azp != s.clientID {
			writeErr(w, 401, "token not issued to expected client")
			return
		}
		h(w, r, owner{Sub: idt.Subject, Name: c.PreferredUsername})
	}
}

type device struct {
	DeviceID         string  `json:"device_id"`
	Name             string  `json:"name"`
	Status           string  `json:"status"`
	CreatedAt        string  `json:"created_at"`
	LastRegisteredAt string  `json:"last_registered_at"`
	RevokedAt        *string `json:"revoked_at,omitempty"`
	ownerSub         string
	userPub          string
}

const deviceCols = `device_id, name, status, created_at, last_registered_at, revoked_at, owner_sub, user_pub`

func scanDevice(row interface{ Scan(...any) error }) (*device, error) {
	var d device
	err := row.Scan(&d.DeviceID, &d.Name, &d.Status, &d.CreatedAt, &d.LastRegisteredAt, &d.RevokedAt, &d.ownerSub, &d.userPub)
	return &d, err
}

type registerReq struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
}

// register is idempotent: re-registering a device you own (even one you
// unregistered earlier) mints new creds and revokes the previous ones. Another owner's device id yields 409.
func (s *server) register(w http.ResponseWriter, r *http.Request, o owner) {
	var req registerReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if !uuidRe.MatchString(req.DeviceID) {
		writeErr(w, 400, "device_id must be a lowercase uuid")
		return
	}
	if !nameRe.MatchString(req.Name) {
		writeErr(w, 400, "name must be 1-64 chars of letters, digits, space, _ . -")
		return
	}

	existing, err := scanDevice(s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE device_id=?`, req.DeviceID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		existing = nil
	case err != nil:
		writeErr(w, 500, "registry error")
		return
	}

	if existing != nil && existing.ownerSub != o.Sub {
		writeErr(w, 409, "device already registered by another owner")
		return
	}
	if existing == nil {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM devices WHERE owner_sub=? AND status='active'`, o.Sub).Scan(&n); err != nil {
			writeErr(w, 500, "registry error")
			return
		}
		if n >= s.maxPerUser {
			writeErr(w, 429, "device quota reached")
			return
		}
	}

	userPub, creds, err := s.trust.MintDeviceCreds(req.DeviceID)
	if err != nil {
		log.Printf("mint %s: %v", req.DeviceID, err)
		writeErr(w, 500, "could not mint credentials")
		return
	}

	// Rotating creds: the old identity must stop working. Do this before
	// touching the registry so a failure leaves everything unchanged.
	if existing != nil && existing.Status == "active" {
		if err := s.trust.Revoke(existing.userPub); err != nil {
			log.Printf("revoke old key of %s: %v", req.DeviceID, err)
			writeErr(w, 502, "could not revoke previous credentials on nats-remote")
			return
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if existing == nil {
		_, err = s.db.Exec(`INSERT INTO devices (device_id,name,owner_sub,owner_name,user_pub,status,created_at,last_registered_at)
			VALUES (?,?,?,?,?, 'active', ?, ?)`, req.DeviceID, req.Name, o.Sub, o.Name, userPub, now, now)
	} else {
		// Also reactivates a device its owner previously unregistered.
		_, err = s.db.Exec(`UPDATE devices SET name=?, user_pub=?, last_registered_at=?, status='active', revoked_at=NULL WHERE device_id=?`,
			req.Name, userPub, now, req.DeviceID)
	}
	if err != nil {
		writeErr(w, 500, "registry error")
		return
	}

	log.Printf("registered device=%s name=%q owner=%s", req.DeviceID, req.Name, o.Name)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{
		"device_id": req.DeviceID,
		"name":      req.Name,
		"leaf_url":  s.leafURL,
		"creds":     creds,
	})
}

func (s *server) get(w http.ResponseWriter, r *http.Request, o owner) {
	d, err := scanDevice(s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE device_id=? AND owner_sub=?`, r.PathValue("id"), o.Sub))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "not found")
		return
	} else if err != nil {
		writeErr(w, 500, "registry error")
		return
	}
	writeJSON(w, 200, d)
}

func (s *server) list(w http.ResponseWriter, r *http.Request, o owner) {
	rows, err := s.db.Query(`SELECT `+deviceCols+` FROM devices WHERE owner_sub=? ORDER BY created_at`, o.Sub)
	if err != nil {
		writeErr(w, 500, "registry error")
		return
	}
	defer rows.Close()
	out := []*device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			writeErr(w, 500, "registry error")
			return
		}
		out = append(out, d)
	}
	writeJSON(w, 200, out)
}

func (s *server) deregister(w http.ResponseWriter, r *http.Request, o owner) {
	d, err := scanDevice(s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE device_id=? AND owner_sub=?`, r.PathValue("id"), o.Sub))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "not found")
		return
	} else if err != nil {
		writeErr(w, 500, "registry error")
		return
	}
	if d.Status != "revoked" {
		if err := s.trust.Revoke(d.userPub); err != nil {
			log.Printf("revoke %s: %v", d.DeviceID, err)
			writeErr(w, 502, "could not revoke on nats-remote")
			return
		}
		if _, err := s.db.Exec(`UPDATE devices SET status='revoked', revoked_at=? WHERE device_id=?`,
			time.Now().UTC().Format(time.RFC3339), d.DeviceID); err != nil {
			writeErr(w, 500, "registry error")
			return
		}
	}
	log.Printf("revoked device=%s owner=%s", d.DeviceID, o.Name)
	w.WriteHeader(204)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
