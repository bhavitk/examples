package main

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

func main() {
	role := os.Getenv("ROLE")
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}

	var opts []nats.Option
	if creds := os.Getenv("NATS_CREDS"); creds != "" {
		opts = append(opts, nats.UserCredentials(creds))
	} else if user := os.Getenv("NATS_USER"); user != "" {
		opts = append(opts, nats.UserInfo(user, os.Getenv("NATS_PASS")))
	}

	nc, err := connectWithRetry(natsURL, opts...)
	if err != nil {
		log.Fatalf("failed to connect to nats at %s: %v", natsURL, err)
	}
	defer nc.Close()

	switch role {
	case "remote":
		runRemote(nc)
	case "device":
		deviceID := os.Getenv("DEVICE_ID")
		if deviceID == "" {
			log.Fatal("DEVICE_ID must be set for ROLE=device")
		}
		runDevice(nc, deviceID)
	default:
		log.Fatalf("unknown ROLE %q (expected 'remote' or 'device')", role)
	}
}

func connectWithRetry(url string, opts ...nats.Option) (*nats.Conn, error) {
	var nc *nats.Conn
	var err error
	for i := 0; i < 20; i++ {
		nc, err = nats.Connect(url, opts...)
		if err == nil {
			return nc, nil
		}
		log.Printf("connect to %s failed (attempt %d/20): %v", url, i+1, err)
		time.Sleep(2 * time.Second)
	}
	return nil, err
}

// runRemote subscribes to all device outgoing data, auto-discovering device
// IDs from the subjects it observes, and periodically sends a targeted
// message to each known device's incoming subject.
func runRemote(nc *nats.Conn) {
	var mu sync.Mutex
	devices := make(map[string]bool)

	_, err := nc.Subscribe("device.*.outgoing.data", func(msg *nats.Msg) {
		log.Printf("[remote] received subject=%s payload=%s", msg.Subject, string(msg.Data))

		id := deviceIDFromOutgoingSubject(msg.Subject)
		if id == "" {
			return
		}
		mu.Lock()
		isNew := !devices[id]
		devices[id] = true
		mu.Unlock()
		if isNew {
			log.Printf("[remote] discovered device %q", id)
		}
	})
	if err != nil {
		log.Fatalf("[remote] subscribe failed: %v", err)
	}
	log.Println("[remote] subscribed to device.*.outgoing.data")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		mu.Lock()
		ids := make([]string, 0, len(devices))
		for id := range devices {
			ids = append(ids, id)
		}
		mu.Unlock()

		for _, id := range ids {
			subject := fmt.Sprintf("device.%s.incoming.data", id)
			payload := fmt.Sprintf(`{"name":"%s","command":"ping","value":%d}`, id, rand.Intn(1000))
			if err := nc.Publish(subject, []byte(payload)); err != nil {
				log.Printf("[remote] publish to %s failed: %v", subject, err)
				continue
			}
			log.Printf("[remote] sent subject=%s payload=%s", subject, payload)
		}
	}
}

// deviceIDFromOutgoingSubject extracts <id> from "device.<id>.outgoing.data".
func deviceIDFromOutgoingSubject(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) != 4 || parts[0] != "device" || parts[2] != "outgoing" || parts[3] != "data" {
		return ""
	}
	return parts[1]
}

// runDevice publishes random outgoing data for its own device id and
// subscribes only to its own incoming subject.
func runDevice(nc *nats.Conn, deviceID string) {
	incomingSubject := fmt.Sprintf("device.%s.incoming.data", deviceID)
	_, err := nc.Subscribe(incomingSubject, func(msg *nats.Msg) {
		log.Printf("[%s] received incoming subject=%s payload=%s", deviceID, msg.Subject, string(msg.Data))
	})
	if err != nil {
		log.Fatalf("[%s] subscribe failed: %v", deviceID, err)
	}
	log.Printf("[%s] subscribed to %s", deviceID, incomingSubject)

	outgoingSubject := fmt.Sprintf("device.%s.outgoing.data", deviceID)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		payload := fmt.Sprintf(`{"name":"%s","value":%d}`, deviceID, rand.Intn(1000))
		if err := nc.Publish(outgoingSubject, []byte(payload)); err != nil {
			log.Printf("[%s] publish failed: %v", deviceID, err)
			continue
		}
		log.Printf("[%s] published subject=%s payload=%s", deviceID, outgoingSubject, payload)
	}
}
