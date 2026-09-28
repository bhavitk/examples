// natskeys mints the operator/SYS/DEVICES trust chain and a user's .creds
// file directly with github.com/nats-io/jwt/v2 and github.com/nats-io/nkeys,
// writing key material into the same nsc "all-dirs" store layout the nsc CLI
// itself uses (keys/<X>/<XX>/<pubkey>.nk, <operator>/<operator>.jwt,
// <operator>/accounts/<name>/<name>.jwt) so scripts/provision-device stays
// able to read it. No nsc CLI or Docker required.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nats-io/jwt/v2"
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

// storeSeed writes an nkey's seed into the store's sharded key tree:
// keys/<pub[0]>/<pub[1:3]>/<pub>.nk - the same layout `nsc` itself uses.
func storeSeed(storeDir string, kp nkeys.KeyPair) error {
	pub, err := kp.PublicKey()
	if err != nil {
		return err
	}
	seed, err := kp.Seed()
	if err != nil {
		return err
	}
	path := filepath.Join(storeDir, "keys", string(pub[0]), pub[1:3], pub+".nk")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, seed, 0o600)
}

// storeAccountJWT writes an account's JWT under
// <storeDir>/<operatorName>/accounts/<accountName>/<accountName>.jwt.
func storeAccountJWT(storeDir, operatorName, accountName, token string) error {
	path := filepath.Join(storeDir, operatorName, "accounts", accountName, accountName+".jwt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token), 0o644)
}

// storeOperatorJWT writes the operator's JWT under
// <storeDir>/<operatorName>/<operatorName>.jwt.
func storeOperatorJWT(storeDir, operatorName, token string) error {
	path := filepath.Join(storeDir, operatorName, operatorName+".jwt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token), 0o644)
}

// mintUserCreds creates a new user identity under an account, scoped to the
// given pub/sub allow subjects, signed by the account's own key, and returns
// it formatted as a .creds file body. Equivalent to:
//
//	nsc add user -a <account> "<name>" --allow-pub "..." --allow-sub "..."
//	nsc generate creds -a <account> -n "<name>"
func mintUserCreds(accountKP nkeys.KeyPair, name string, pubAllow, subAllow []string) (string, error) {
	userKP, err := nkeys.CreateUser()
	if err != nil {
		return "", fmt.Errorf("create user keypair: %w", err)
	}
	userPub, err := userKP.PublicKey()
	if err != nil {
		return "", err
	}
	userSeed, err := userKP.Seed()
	if err != nil {
		return "", err
	}

	claims := jwt.NewUserClaims(userPub)
	claims.Name = name
	claims.Pub.Allow.Add(pubAllow...)
	claims.Sub.Allow.Add(subAllow...)

	token, err := claims.Encode(accountKP)
	if err != nil {
		return "", fmt.Errorf("sign user jwt: %w", err)
	}

	return fmt.Sprintf(credsTemplate, token, userSeed), nil
}
