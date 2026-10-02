// local-backend runs next to a device's local NATS server. `init` prepares the
// NATS config + secrets once (so nats-local can start unregistered); `serve`
// runs the UI/backend: it publishes device data to local NATS the whole time,
// and registers the device with the remote (Keycloak login -> registration-api
// -> leaf.conf -> restart nats-local) on demand.
package main

import (
	"log"
	"os"
	"strconv"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: local-backend init|serve")
	}
	dataDir := env("DATA_DIR", "/data")
	switch os.Args[1] {
	case "init":
		if err := runInit(dataDir, env("LOCAL_NAME", "L1")); err != nil {
			log.Fatalf("init: %v", err)
		}
	case "serve":
		runServe(dataDir)
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}
