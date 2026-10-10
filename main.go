// FAK BOT Pairing Service — web-based WhatsApp pairing.
// User enters phone number → gets 8-digit code → enters it on their phone
// → downloads the session file → imports it into the FAK BOT app.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "github.com/mattn/go-sqlite3"
)

type pairSession struct {
	id        string
	phone     string
	client    *whatsmeow.Client
	container *sqlstore.Container
	dbPath    string
	code      string
	status    string // waiting | connected | failed
	createdAt time.Time
	mu        sync.Mutex
}

var (
	sessions   = map[string]*pairSession{}
	sessionsMu sync.Mutex
)

func newSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func getSession(id string) *pairSession {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[id]
}

func cleanupSessions() {
	for {
		time.Sleep(60 * time.Second)
		now := time.Now()
		sessionsMu.Lock()
		for id, s := range sessions {
			s.mu.Lock()
			expired := now.Sub(s.createdAt) > 6*time.Minute
			s.mu.Unlock()
			if expired {
				s.client.Disconnect()
				os.Remove(s.dbPath)
				delete(sessions, id)
				fmt.Printf("[pairing] cleaned up expired session %s\n", id)
			}
		}
		sessionsMu.Unlock()
	}
}

func handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	phone := strings.TrimSpace(req.Phone)
	phone = strings.Map(func(c rune) rune {
		if c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, phone)
	if len(phone) < 8 || len(phone) > 15 {
		http.Error(w, "invalid phone number", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	dbPath := filepath.Join(os.TempDir(), "fakpair_"+newSessionID()+".db")
	container, err := sqlstore.New(ctx, "sqlite3",
		fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", dbPath), waLog.Noop)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	device := container.NewDevice()
	client := whatsmeow.NewClient(device, waLog.Noop)

	s := &pairSession{
		id:        newSessionID(),
		phone:     phone,
		client:    client,
		container: container,
		dbPath:    dbPath,
		status:    "waiting",
		createdAt: time.Now(),
	}
	client.AddEventHandler(func(evt interface{}) {
		if _, ok := evt.(*events.Connected); ok {
			s.mu.Lock()
			s.status = "connected"
			s.mu.Unlock()
			fmt.Printf("[pairing] session %s connected (%s)\n", s.id, phone)
		}
	})

	if err := client.Connect(); err != nil {
		os.Remove(dbPath)
		http.Error(w, "connect failed", http.StatusInternalServerError)
		return
	}
	// Small delay to let the connection establish before requesting code
	time.Sleep(2 * time.Second)

	code, err := client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		client.Disconnect()
		os.Remove(dbPath)
		http.Error(w, "pairing failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Format as XXXX-XXXX
	if len(code) == 8 {
		code = code[:4] + "-" + code[4:]
	}
	s.code = code

	sessionsMu.Lock()
	sessions[s.id] = s
	sessionsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"session_id": s.id,
		"code":       code,
	})
	fmt.Printf("[pairing] new session %s for %s\n", s.id, phone)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/status/")
	s := getSession(id)
	if s == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	status := s.status
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/download/")
	s := getSession(id)
	if s == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	status := s.status
	s.mu.Unlock()
	if status != "connected" {
		http.Error(w, "not paired yet", http.StatusBadRequest)
		return
	}
	// Disconnect first so SQLite is cleanly closed
	s.client.Disconnect()
	time.Sleep(500 * time.Millisecond)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="fakbot-session.db"`)
	http.ServeFile(w, r, s.dbPath)

	// Cleanup after download
	sessionsMu.Lock()
	delete(sessions, id)
	sessionsMu.Unlock()
	os.Remove(s.dbPath)
	fmt.Printf("[pairing] session %s downloaded and cleaned\n", id)
}

func main() {
	go cleanupSessions()

	http.HandleFunc("/api/pair", handlePair)
	http.HandleFunc("/api/status/", handleStatus)
	http.HandleFunc("/api/download/", handleDownload)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	// Health check for Render / UptimeRobot
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("[pairing] listening on :%s\n", port)
	http.ListenAndServe(":"+port, nil)
}
