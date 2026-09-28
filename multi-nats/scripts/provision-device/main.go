// provision-device is a Go re-implementation of scripts/provision-device.sh.
//
// It mints a User JWT under the (already-trusted) DEVICES account, restricted
// to device.<id>.outgoing.data / device.<id>.incoming.data, generates its
// .creds file (via natsauth.go, using the nsc store's own key material
// directly - no nsc CLI or Docker needed for that step), writes the device's
// nats-server config + a docker-compose override, and brings the two new
// containers up via Docker. nats-remote is never touched.
//
// Usage: go run ./scripts/provision-device <device-id>
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

var deviceIDRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// Provisioner holds everything needed to provision a single device and
// exposes each step of the process as its own method, mirroring the stages
// of provision-device.sh.
type Provisioner struct {
	ProjectDir string
	DeviceID   string
}

func main() {
	projectDir := flag.String("project-dir", ".", "path to the multi-nats project root")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-project-dir DIR] <device-id>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	p := &Provisioner{
		ProjectDir: *projectDir,
		DeviceID:   flag.Arg(0),
	}

	if err := p.Run(); err != nil {
		log.Fatalf("provision %s: %v", p.DeviceID, err)
	}
}

// Run executes every provisioning step in order, matching the sequence in
// provision-device.sh.
func (p *Provisioner) Run() error {
	if err := p.ValidateDeviceID(); err != nil {
		return err
	}
	if err := p.CheckResolverConfig(); err != nil {
		return err
	}
	if err := p.EnsureCredsDir(); err != nil {
		return err
	}

	localPass, err := p.GenerateLocalPassword()
	if err != nil {
		return fmt.Errorf("generate local password: %w", err)
	}

	if err := p.ProvisionNatsUser(); err != nil {
		return fmt.Errorf("provision nats user: %w", err)
	}
	if err := p.WriteDeviceConfig(localPass); err != nil {
		return fmt.Errorf("write device config: %w", err)
	}
	if err := p.WriteComposeOverride(localPass); err != nil {
		return fmt.Errorf("write compose override: %w", err)
	}
	if err := p.BringUp(); err != nil {
		return fmt.Errorf("bring up containers: %w", err)
	}

	fmt.Printf("\nDevice %s provisioned and connected.\n", p.DeviceID)
	return nil
}

// ValidateDeviceID rejects empty or unsafe device IDs before they get baked
// into file names, container names, and NATS subjects.
func (p *Provisioner) ValidateDeviceID() error {
	if !deviceIDRe.MatchString(p.DeviceID) {
		return fmt.Errorf("invalid device id %q (must match %s)", p.DeviceID, deviceIDRe.String())
	}
	return nil
}

// CheckResolverConfig ensures scripts/setup-operator.sh has already been run.
func (p *Provisioner) CheckResolverConfig() error {
	path := filepath.Join(p.ProjectDir, "nats-config", "resolver.conf")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s not found - run scripts/setup-operator.sh first", path)
	}
	return nil
}

// EnsureCredsDir makes sure nsc-data/creds exists before the device's creds
// file is written into it.
func (p *Provisioner) EnsureCredsDir() error {
	return os.MkdirAll(filepath.Join(p.ProjectDir, "nsc-data", "creds"), 0o755)
}

// GenerateLocalPassword produces the random password the device's own
// nats-server uses to authenticate its local client, equivalent to:
//
//	head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n'
func (p *Provisioner) GenerateLocalPassword() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ProvisionNatsUser mints a User JWT under the DEVICES account, scoped to
// this device's subjects, and writes its .creds file into nsc-data/creds -
// using the nsc store's own key material directly (see natsauth.go), no
// nsc CLI or Docker required.
func (p *Provisioner) ProvisionNatsUser() error {
	storeDir := filepath.Join(p.ProjectDir, "nsc-data", "store")
	creds, err := mintDeviceUserCreds(storeDir, "DEVICES", p.DeviceID)
	if err != nil {
		return err
	}

	path := filepath.Join(p.ProjectDir, "nsc-data", "creds", p.DeviceID+".creds")
	return os.WriteFile(path, []byte(creds), 0o600)
}

const deviceConfigTemplate = `server_name: device-%[1]s

port: 4222
http_port: 8222

leafnodes {
  remotes = [
    { url: "leaf://nats-remote:7422", credentials: "/etc/nats/leaf.creds" }
  ]
}

authorization {
  users = [
    {
      user: "%[1]s"
      password: "%[2]s"
      permissions: {
        publish: { allow: ["device.%[1]s.outgoing.data"] }
        subscribe: { allow: ["device.%[1]s.incoming.data"] }
      }
    }
  ]
}
`

// WriteDeviceConfig writes the device's nats-server config file.
func (p *Provisioner) WriteDeviceConfig(localPass string) error {
	path := filepath.Join(p.ProjectDir, "nats-config", fmt.Sprintf("device-%s.conf", p.DeviceID))
	content := fmt.Sprintf(deviceConfigTemplate, p.DeviceID, localPass)
	return os.WriteFile(path, []byte(content), 0o644)
}

const composeOverrideTemplate = `services:
  nats-%[1]s:
    image: nats:2.10-alpine
    container_name: nats-%[1]s
    command: ["-c", "/etc/nats/device.conf"]
    volumes:
      - ./nats-config/device-%[1]s.conf:/etc/nats/device.conf:ro
      - ./nsc-data/creds/%[1]s.creds:/etc/nats/leaf.creds:ro
    networks:
      default: {}

  device-%[1]s-client:
    build: ./app
    container_name: device-%[1]s-client
    environment:
      ROLE: device
      DEVICE_ID: %[1]s
      NATS_URL: nats://nats-%[1]s:4222
      NATS_USER: %[1]s
      NATS_PASS: %[2]s
    depends_on:
      - nats-%[1]s
    networks:
      default: {}

networks:
  default:
    name: multi-nats_default
    external: true
`

// WriteComposeOverride writes the docker-compose.<id>.yml override that
// defines this device's own nats-server and client containers.
func (p *Provisioner) WriteComposeOverride(localPass string) error {
	path := filepath.Join(p.ProjectDir, fmt.Sprintf("docker-compose.%s.yml", p.DeviceID))
	content := fmt.Sprintf(composeOverrideTemplate, p.DeviceID, localPass)
	return os.WriteFile(path, []byte(content), 0o644)
}

// BringUp starts the device's containers via the generated compose override.
// nats-remote is never restarted.
func (p *Provisioner) BringUp() error {
	fmt.Printf("Bringing up nats-%s and device-%s-client (nats-remote is NOT touched)...\n", p.DeviceID, p.DeviceID)
	return p.runCompose("-f", fmt.Sprintf("docker-compose.%s.yml", p.DeviceID), "up", "-d", "--build")
}

// runDocker runs a docker CLI command with the project directory as its
// working directory, streaming output to the current process's stdout/stderr.
func (p *Provisioner) runDocker(args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Dir = p.ProjectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runCompose runs `docker compose <args>` with the project directory as its
// working directory.
func (p *Provisioner) runCompose(args ...string) error {
	return p.runDocker(append([]string{"compose"}, args...)...)
}
