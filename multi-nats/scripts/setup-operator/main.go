// setup-operator is a Go re-implementation of scripts/setup-operator.sh.
//
// One-time setup: creates the Operator, SYS account, and DEVICES account,
// then writes nats-config/resolver.conf (mem-resolver preload) for
// nats-remote, plus a "remote" user under DEVICES so remote-client can
// connect. All key generation and JWT signing happens directly via
// github.com/nats-io/jwt/v2 and github.com/nats-io/nkeys (see natskeys.go) -
// no nsc CLI or Docker required.
//
// This is the ONLY step that ever touches nats-remote's config. After this,
// new devices are provisioned with scripts/provision-device and nats-remote
// is never restarted again.
//
// Usage: go run ./scripts/setup-operator
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

const operatorName = "multinats"

// Setup holds everything needed to bootstrap the operator/account trust
// chain and exposes each step of the process as its own method, mirroring
// the stages of setup-operator.sh.
type Setup struct {
	ProjectDir string

	operatorKP  nkeys.KeyPair
	operatorPub string
	signingKP   nkeys.KeyPair
	signingPub  string

	sysKP  nkeys.KeyPair
	sysPub string

	devicesKP  nkeys.KeyPair
	devicesPub string
}

func main() {
	projectDir := flag.String("project-dir", ".", "path to the multi-nats project root")
	flag.Parse()

	s := &Setup{ProjectDir: *projectDir}
	if err := s.Run(); err != nil {
		log.Fatalf("setup-operator: %v", err)
	}
}

// Run executes every setup step in order, matching the sequence in
// setup-operator.sh.
func (s *Setup) Run() error {
	if err := s.EnsureDirs(); err != nil {
		return fmt.Errorf("ensure dirs: %w", err)
	}
	if err := s.GenerateOperatorKeys(); err != nil {
		return fmt.Errorf("generate operator keys: %w", err)
	}
	if err := s.GenerateAccountKeys(); err != nil {
		return fmt.Errorf("generate account keys: %w", err)
	}

	operatorJWT, err := s.BuildOperatorJWT()
	if err != nil {
		return fmt.Errorf("build operator jwt: %w", err)
	}
	sysJWT, err := s.BuildAccountJWT("SYS", s.sysKP, s.sysPub)
	if err != nil {
		return fmt.Errorf("build SYS account jwt: %w", err)
	}
	devicesJWT, err := s.BuildAccountJWT("DEVICES", s.devicesKP, s.devicesPub)
	if err != nil {
		return fmt.Errorf("build DEVICES account jwt: %w", err)
	}

	if err := s.StoreKeys(); err != nil {
		return fmt.Errorf("store keys: %w", err)
	}
	if err := s.StoreJWTs(operatorJWT, sysJWT, devicesJWT); err != nil {
		return fmt.Errorf("store jwts: %w", err)
	}
	if err := s.WriteResolverConfig(operatorJWT, sysJWT, devicesJWT); err != nil {
		return fmt.Errorf("write resolver.conf: %w", err)
	}
	if err := s.ProvisionRemoteUser(); err != nil {
		return fmt.Errorf("provision remote user: %w", err)
	}

	fmt.Println("--- operator/account setup complete ---")
	fmt.Printf("operator %q: %s\n", operatorName, s.operatorPub)
	fmt.Printf("account  %q: %s\n", "SYS", s.sysPub)
	fmt.Printf("account  %q: %s\n", "DEVICES", s.devicesPub)
	fmt.Println()
	fmt.Println("resolver.conf written to nats-config/resolver.conf")
	fmt.Println("Next: include it from nats-config/remote.conf, then start/restart nats-remote ONE more time to pick it up.")
	return nil
}

// EnsureDirs makes sure nsc-data and nats-config exist.
func (s *Setup) EnsureDirs() error {
	if err := os.MkdirAll(filepath.Join(s.ProjectDir, "nsc-data"), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(s.ProjectDir, "nats-config"), 0o755)
}

// GenerateOperatorKeys creates the operator's identity keypair plus a
// signing keypair, equivalent to `nsc add operator --generate-signing-key`.
func (s *Setup) GenerateOperatorKeys() error {
	var err error
	if s.operatorKP, err = nkeys.CreateOperator(); err != nil {
		return err
	}
	if s.operatorPub, err = s.operatorKP.PublicKey(); err != nil {
		return err
	}
	if s.signingKP, err = nkeys.CreateOperator(); err != nil {
		return err
	}
	if s.signingPub, err = s.signingKP.PublicKey(); err != nil {
		return err
	}
	return nil
}

// GenerateAccountKeys creates the identity keypairs for the SYS and DEVICES
// accounts, equivalent to `nsc add account SYS` / `nsc add account DEVICES`.
func (s *Setup) GenerateAccountKeys() error {
	var err error
	if s.sysKP, err = nkeys.CreateAccount(); err != nil {
		return err
	}
	if s.sysPub, err = s.sysKP.PublicKey(); err != nil {
		return err
	}
	if s.devicesKP, err = nkeys.CreateAccount(); err != nil {
		return err
	}
	if s.devicesPub, err = s.devicesKP.PublicKey(); err != nil {
		return err
	}
	return nil
}

// BuildOperatorJWT builds and self-signs the operator claim, declaring its
// signing key and the SYS account as the system account.
func (s *Setup) BuildOperatorJWT() (string, error) {
	claims := jwt.NewOperatorClaims(s.operatorPub)
	claims.Name = operatorName
	claims.SigningKeys.Add(s.signingPub)
	claims.SystemAccount = s.sysPub
	return claims.Encode(s.operatorKP)
}

// BuildAccountJWT builds an account claim for name/accountKP/accountPub,
// gives it its own signing key, and signs it with the operator's signing
// key - equivalent to `nsc add account <name>` + `nsc edit account <name>
// --sk generate`.
func (s *Setup) BuildAccountJWT(name string, accountKP nkeys.KeyPair, accountPub string) (string, error) {
	accountSigningKP, err := nkeys.CreateAccount()
	if err != nil {
		return "", err
	}
	accountSigningPub, err := accountSigningKP.PublicKey()
	if err != nil {
		return "", err
	}
	if err := storeSeed(s.storeDir(), accountSigningKP); err != nil {
		return "", err
	}

	claims := jwt.NewAccountClaims(accountPub)
	claims.Name = name
	claims.SigningKeys.Add(accountSigningPub)
	return claims.Encode(s.signingKP)
}

// StoreKeys writes every generated keypair's seed into the nsc store's key
// tree so scripts/provision-device can find it later.
func (s *Setup) StoreKeys() error {
	for _, kp := range []nkeys.KeyPair{s.operatorKP, s.signingKP, s.sysKP, s.devicesKP} {
		if err := storeSeed(s.storeDir(), kp); err != nil {
			return err
		}
	}
	return nil
}

// StoreJWTs writes the operator and account JWTs into the nsc store layout.
func (s *Setup) StoreJWTs(operatorJWT, sysJWT, devicesJWT string) error {
	if err := storeOperatorJWT(s.storeDir(), operatorName, operatorJWT); err != nil {
		return err
	}
	if err := storeAccountJWT(s.storeDir(), operatorName, "SYS", sysJWT); err != nil {
		return err
	}
	return storeAccountJWT(s.storeDir(), operatorName, "DEVICES", devicesJWT)
}

const resolverConfigTemplate = `// Operator %[1]q
operator: %[2]s

system_account: %[3]s

resolver: MEMORY

resolver_preload: {
  // Account "SYS"
  %[3]s: %[4]s

  // Account "DEVICES"
  %[5]s: %[6]s

}
`

// WriteResolverConfig writes nats-config/resolver.conf, the mem-resolver
// preload nats-remote loads at startup - equivalent to
// `nsc generate config --mem-resolver --sys-account SYS`.
func (s *Setup) WriteResolverConfig(operatorJWT, sysJWT, devicesJWT string) error {
	content := fmt.Sprintf(resolverConfigTemplate,
		operatorName, operatorJWT, s.sysPub, sysJWT, s.devicesPub, devicesJWT)
	path := filepath.Join(s.ProjectDir, "nats-config", "resolver.conf")
	return os.WriteFile(path, []byte(content), 0o644)
}

// ProvisionRemoteUser mints the "remote" user under DEVICES that
// remote-client authenticates with, scoped to every device's subjects.
func (s *Setup) ProvisionRemoteUser() error {
	creds, err := mintUserCreds(s.devicesKP, "remote",
		[]string{"device.*.incoming.data"},
		[]string{"device.*.outgoing.data"},
	)
	if err != nil {
		return err
	}

	credsDir := filepath.Join(s.ProjectDir, "nsc-data", "creds")
	if err := os.MkdirAll(credsDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(credsDir, "remote.creds"), []byte(creds), 0o600)
}

func (s *Setup) storeDir() string {
	return filepath.Join(s.ProjectDir, "nsc-data", "store")
}
