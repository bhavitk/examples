package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed static/index.html
var staticFS embed.FS

var deviceClientRe = regexp.MustCompile(`^device-(.+)-client$`)

type event struct {
	Type      string `json:"type"` // "log" | "device-list"
	Container string `json:"container,omitempty"`
	Role      string `json:"role,omitempty"` // "remote" | "device"
	DeviceID  string `json:"deviceId,omitempty"`
	Line      string `json:"line,omitempty"`
	TS        int64  `json:"ts,omitempty"`
	Devices   []string `json:"devices,omitempty"`
}

type hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]bool
}

func newHub() *hub { return &hub{clients: make(map[*websocket.Conn]bool)} }

func (h *hub) add(c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = true
}

func (h *hub) remove(c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	c.Close()
}

func (h *hub) broadcast(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if err := c.WriteJSON(e); err != nil {
			c.Close()
			delete(h.clients, c)
		}
	}
}

type tailer struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
	seen   map[string]bool // known device IDs
}

func main() {
	h := newHub()
	t := &tailer{active: make(map[string]context.CancelFunc), seen: make(map[string]bool)}

	go t.discoverLoop(h)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data, _ := staticFS.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		h.add(conn)
		t.mu.Lock()
		ids := make([]string, 0, len(t.seen))
		for id := range t.seen {
			ids = append(ids, id)
		}
		t.mu.Unlock()
		conn.WriteJSON(event{Type: "device-list", Devices: ids})

		defer h.remove(conn)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !isValidDeviceID(body.ID) {
			http.Error(w, "invalid device id", http.StatusBadRequest)
			return
		}
		log.Printf("provisioning device %q", body.ID)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			projectDir := os.Getenv("PROJECT_DIR")
			if projectDir == "" {
				projectDir = "."
			}
			cmd := exec.CommandContext(ctx, "sh", "scripts/provision-device.sh", body.ID)
			cmd.Dir = projectDir
			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Printf("provision %s failed: %v\n%s", body.ID, err, out)
				return
			}
			log.Printf("provision %s complete", body.ID)
		}()
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "provisioning", "id": body.ID})
	})

	log.Println("dashboard listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func isValidDeviceID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

// discoverLoop polls docker for relevant containers and starts/stops log tailers.
func (t *tailer) discoverLoop(h *hub) {
	for {
		names := listContainers()
		wanted := make(map[string]bool)
		for _, n := range names {
			if n == "remote-client" {
				wanted[n] = true
			} else if deviceClientRe.MatchString(n) {
				wanted[n] = true
			}
		}

		t.mu.Lock()
		for name := range wanted {
			if _, ok := t.active[name]; ok {
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.active[name] = cancel
			role := "remote"
			deviceID := ""
			if m := deviceClientRe.FindStringSubmatch(name); m != nil {
				role = "device"
				deviceID = m[1]
				if !t.seen[deviceID] {
					t.seen[deviceID] = true
					h.broadcast(event{Type: "device-list", Devices: seenList(t.seen)})
				}
			}
			go tailContainer(ctx, name, role, deviceID, h)
		}
		for name, cancel := range t.active {
			if !wanted[name] {
				cancel()
				delete(t.active, name)
			}
		}
		t.mu.Unlock()

		time.Sleep(2 * time.Second)
	}
}

func seenList(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func listContainers() []string {
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var names []string
	for _, l := range lines {
		if l != "" {
			names = append(names, l)
		}
	}
	return names
}

func tailContainer(ctx context.Context, name, role, deviceID string, h *hub) {
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", "--tail", "0", name)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		h.broadcast(event{
			Type:      "log",
			Container: name,
			Role:      role,
			DeviceID:  deviceID,
			Line:      line,
			TS:        time.Now().UnixMilli(),
		})
	}
	cmd.Wait()
}
