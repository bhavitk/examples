// natsauth mints nsc-compatible user JWTs and .creds files directly with
// github.com/nats-io/jwt/v2 and github.com/nats-io/nkeys, reading key
// material straight out of the nsc store (as created by
// scripts/setup-operator.sh). This replaces the `nsc` CLI for the one thing
// provision-device needs it for, so minting a device's credentials needs no
// Docker and no nsc binary at all.
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

// loadAccountKeyPair locates accountName's own JWT under storeDir (an nsc
// "all-dirs" store, e.g. nsc-data/store) and loads the nkey seed matching
// its identity, so callers can sign new user JWTs for that account.
func loadAccountKeyPair(storeDir, accountName string) (nkeys.KeyPair, error) {
	matches, err := filepath.Glob(filepath.Join(storeDir, "*", "accounts", accountName, accountName+".jwt"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no account jwt found for %q under %s - run scripts/setup-operator.sh first", accountName, storeDir)
	}

	data, err := os.ReadFile(matches[0])
	if err != nil {
		return nil, err
	}

	claims, err := jwt.DecodeAccountClaims(string(data))
	if err != nil {
		return nil, fmt.Errorf("decode %s account jwt: %w", accountName, err)
	}

	seed, err := loadSeed(storeDir, claims.Subject)
	if err != nil {
		return nil, err
	}

	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("load %s account keypair: %w", accountName, err)
	}
	return kp, nil
}

// loadSeed reads an nkey seed from the store's sharded key tree:
// keys/<pub[0]>/<pub[1:3]>/<pub>.nk - the same layout `nsc` itself uses.
func loadSeed(storeDir, pub string) ([]byte, error) {
	if len(pub) < 3 {
		return nil, fmt.Errorf("invalid public key %q", pub)
	}
	path := filepath.Join(storeDir, "keys", string(pub[0]), pub[1:3], pub+".nk")
	seed, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key seed for %s: %w", pub, err)
	}
	return seed, nil
}

// mintDeviceUserCreds creates a new user identity under accountName, scoped
// to publish device.<deviceID>.outgoing.data and subscribe
// device.<deviceID>.incoming.data, signed by that account's own key, and
// returns it already formatted as a .creds file body. Equivalent to:
//
//	nsc add user -a <accountName> "<deviceID>" \
//	  --allow-pub "device.<deviceID>.outgoing.data" \
//	  --allow-sub "device.<deviceID>.incoming.data"
//	nsc generate creds -a <accountName> -n "<deviceID>"
func mintDeviceUserCreds(storeDir, accountName, deviceID string) (string, error) {
	accountKP, err := loadAccountKeyPair(storeDir, accountName)
	if err != nil {
		return "", err
	}

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
	claims.Name = deviceID
	claims.Pub.Allow.Add(fmt.Sprintf("device.%s.outgoing.data", deviceID))
	claims.Sub.Allow.Add(fmt.Sprintf("device.%s.incoming.data", deviceID))

	token, err := claims.Encode(accountKP)
	if err != nil {
		return "", fmt.Errorf("sign user jwt: %w", err)
	}

	return fmt.Sprintf(credsTemplate, token, userSeed), nil
}
