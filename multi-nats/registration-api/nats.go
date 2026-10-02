package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

const credsTemplate = `-----BEGIN NATS USER JWT-----
%s
------END NATS USER JWT------

*************************** IMPORTANT ***************************
NKEY Seed printed below can be used to sign and prove identity.
NKEYs are sensitive and should be treated as secrets.

-----BEGIN USER NKEY SEED-----
%s
------END USER NKEY SEED------

*************************************************************
`

// Trust holds the key material registration-api needs, read from the nsc
// store written by scripts/setup-operator:
//
//   - the DEVICES account signing key, used to mint device user JWTs
//     (IssuerAccount points at the account identity, so the identity key
//     itself is never loaded here);
//   - the operator signing key, used ONLY to re-sign the DEVICES account JWT
//     when a revocation is recorded (POC compromise: a production setup
//     would keep this behind a separate, tightly held service).
type Trust struct {
	mu sync.Mutex

	storeDir   string
	operator   string // operator name (dir under store)
	accountJWT string // path to DEVICES account jwt in the store

	devicesPub string
	signingKP  nkeys.KeyPair // account signing key
	operatorKP nkeys.KeyPair // operator signing key
	sysCreds   string
	natsURL    string
}

func LoadTrust(storeDir, accountName, sysCreds, natsURL string) (*Trust, error) {
	matches, err := filepath.Glob(filepath.Join(storeDir, "*", "accounts", accountName, accountName+".jwt"))
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("no %s account jwt under %s - run scripts/setup-operator first", accountName, storeDir)
	}
	t := &Trust{
		storeDir:   storeDir,
		accountJWT: matches[0],
		operator:   filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(matches[0])))),
		sysCreds:   sysCreds,
		natsURL:    natsURL,
	}

	claims, err := t.readAccountClaims()
	if err != nil {
		return nil, err
	}
	t.devicesPub = claims.Subject
	if len(claims.SigningKeys) == 0 {
		return nil, fmt.Errorf("account %s has no signing key", accountName)
	}
	if t.signingKP, err = t.loadSeed(claims.SigningKeys.Keys()[0]); err != nil {
		return nil, fmt.Errorf("account signing key: %w", err)
	}

	opData, err := os.ReadFile(filepath.Join(storeDir, t.operator, t.operator+".jwt"))
	if err != nil {
		return nil, err
	}
	opClaims, err := jwt.DecodeOperatorClaims(string(opData))
	if err != nil {
		return nil, fmt.Errorf("decode operator jwt: %w", err)
	}
	if len(opClaims.SigningKeys) == 0 {
		return nil, fmt.Errorf("operator has no signing key")
	}
	if t.operatorKP, err = t.loadSeed(opClaims.SigningKeys[0]); err != nil {
		return nil, fmt.Errorf("operator signing key: %w", err)
	}
	return t, nil
}

func (t *Trust) loadSeed(pub string) (nkeys.KeyPair, error) {
	path := filepath.Join(t.storeDir, "keys", string(pub[0]), pub[1:3], pub+".nk")
	seed, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return nkeys.FromSeed(seed)
}

func (t *Trust) readAccountClaims() (*jwt.AccountClaims, error) {
	data, err := os.ReadFile(t.accountJWT)
	if err != nil {
		return nil, err
	}
	return jwt.DecodeAccountClaims(string(data))
}

// MintDeviceCreds creates a fresh user identity scoped to this device's own
// subjects, with no expiry, and returns its public key and .creds body.
func (t *Trust) MintDeviceCreds(deviceID string) (userPub, creds string, err error) {
	userKP, err := nkeys.CreateUser()
	if err != nil {
		return "", "", err
	}
	userPub, err = userKP.PublicKey()
	if err != nil {
		return "", "", err
	}
	seed, err := userKP.Seed()
	if err != nil {
		return "", "", err
	}

	claims := jwt.NewUserClaims(userPub)
	claims.Name = deviceID
	claims.IssuerAccount = t.devicesPub
	claims.Pub.Allow.Add(fmt.Sprintf("device.%s.outgoing.data", deviceID))
	claims.Sub.Allow.Add(fmt.Sprintf("device.%s.incoming.data", deviceID))
	claims.Limits.Payload = 64 * 1024 // 64KB max message per device

	token, err := claims.Encode(t.signingKP)
	if err != nil {
		return "", "", err
	}
	return userPub, fmt.Sprintf(credsTemplate, token, seed), nil
}

// Revoke adds userPub to the DEVICES account revocations, re-signs the
// account JWT with the operator signing key, pushes it to nats-remote's full
// resolver (so it takes effect immediately and connected leafnodes using that
// key are disconnected), then persists it in the store.
func (t *Trust) Revoke(userPub string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	claims, err := t.readAccountClaims()
	if err != nil {
		return err
	}
	if claims.Revocations == nil {
		claims.Revocations = jwt.RevocationList{}
	}
	claims.Revocations.Revoke(userPub, time.Now())

	token, err := claims.Encode(t.operatorKP)
	if err != nil {
		return err
	}
	// Push first: if nats-remote rejects/is unreachable the store must not
	// drift ahead of what the resolver actually enforces.
	if err := t.pushClaims(token); err != nil {
		return err
	}
	return os.WriteFile(t.accountJWT, []byte(token), 0o644)
}

func (t *Trust) pushClaims(token string) error {
	nc, err := nats.Connect(t.natsURL, nats.UserCredentials(t.sysCreds), nats.Timeout(5*time.Second))
	if err != nil {
		return fmt.Errorf("connect sys: %w", err)
	}
	defer nc.Close()
	msg, err := nc.Request("$SYS.REQ.CLAIMS.UPDATE", []byte(token), 5*time.Second)
	if err != nil {
		return fmt.Errorf("push claims: %w", err)
	}
	if strings.Contains(string(msg.Data), `"error"`) {
		return fmt.Errorf("resolver rejected claims: %s", msg.Data)
	}
	return nil
}
