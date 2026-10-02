package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// Device is the persisted identity of this local site. The id is generated
// once on first start and never changes, registered or not.
type Device struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Registered   bool   `json:"registered"`
	RegisteredAt string `json:"registered_at,omitempty"`
}

type Secrets struct {
	NatsUser string `json:"nats_user"`
	NatsPass string `json:"nats_pass"`
}

func devicePath(dataDir string) string  { return filepath.Join(dataDir, "device.json") }
func secretsPath(dataDir string) string { return filepath.Join(dataDir, "secrets.json") }
func natsDir(dataDir string) string     { return filepath.Join(dataDir, "nats") }

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

const natsConfTemplate = `server_name: device-%[1]s

port: 4222
http_port: 8222

# Local client auth. The device may only touch its own subjects.
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

# Empty until the device is registered; written by local-backend.
include leaf.conf
`

// supervisor restarts nats-server when local-backend drops a "restart" marker
// file. NATS 2.10 cannot add/change a leafnode remote on config reload, and
// has no system-subject shutdown, so a restart is how a new leaf.conf takes
// effect - this avoids giving local-backend the docker socket.
const supervisorScript = `#!/bin/sh
cd /nats
pid=""
trap '[ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null; exit 0' TERM INT
while true; do
  nats-server -c /nats/nats.conf &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    if [ -f /nats/restart ]; then
      rm -f /nats/restart
      kill -TERM "$pid"
    fi
    sleep 1
  done
  wait "$pid"
done
`

// runInit is idempotent: it creates identity + local NATS config once.
func runInit(dataDir, name string) error {
	if err := os.MkdirAll(natsDir(dataDir), 0o755); err != nil {
		return err
	}

	var dev Device
	if err := readJSON(devicePath(dataDir), &dev); errors.Is(err, os.ErrNotExist) {
		dev = Device{ID: uuid.NewString(), Name: name}
		if err := writeJSON(devicePath(dataDir), dev); err != nil {
			return err
		}
		log.Printf("generated device id %s (name %s)", dev.ID, dev.Name)
	} else if err != nil {
		return err
	}

	var sec Secrets
	if err := readJSON(secretsPath(dataDir), &sec); errors.Is(err, os.ErrNotExist) {
		sec = Secrets{NatsUser: dev.ID, NatsPass: randHex(16)}
		if err := writeJSON(secretsPath(dataDir), sec); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	nd := natsDir(dataDir)
	if err := writeFileAtomic(filepath.Join(nd, "nats.conf"),
		[]byte(fmt.Sprintf(natsConfTemplate, sec.NatsUser, sec.NatsPass)), 0o644); err != nil {
		return err
	}
	leaf := filepath.Join(nd, "leaf.conf")
	if _, err := os.Stat(leaf); errors.Is(err, os.ErrNotExist) {
		if err := writeFileAtomic(leaf, nil, 0o644); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(filepath.Join(nd, "run.sh"), []byte(supervisorScript), 0o755); err != nil {
		return err
	}
	log.Printf("init done for device %s", dev.ID)
	return nil
}
