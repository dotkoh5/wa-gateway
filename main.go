package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Actual wacli --json output format
type WacliMessage struct {
	ChatJID     string `json:"ChatJID"`
	ChatName    string `json:"ChatName"`
	MsgID       string `json:"MsgID"`
	SenderJID   string `json:"SenderJID"`
	Timestamp   string `json:"Timestamp"` // RFC3339
	FromMe      bool   `json:"FromMe"`
	Text        string `json:"Text"`
	DisplayText string `json:"DisplayText"`
	MediaType   string `json:"MediaType"`
	Snippet     string `json:"Snippet"`
}

// wacli messages list --json response wrapper
type WacliMessagesResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Messages []WacliMessage `json:"messages"`
	} `json:"data"`
}

// Payload we POST to OtterClawd
type WebhookPayload struct {
	From      string `json:"from"`
	Text      string `json:"text"`
	PushName  string `json:"pushName,omitempty"`
	ChatType  string `json:"chatType"`
	GroupID   string `json:"groupId,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

type SendRequest struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

type SendResponse struct {
	Success   bool   `json:"success"`
	MessageID string `json:"messageId,omitempty"`
	Error     string `json:"error,omitempty"`
}

var (
	config Config

	syncStatus struct {
		mu        sync.RWMutex
		connected bool
		lastMsg   time.Time
		lastSync  time.Time
	}

	seenMessages struct {
		mu  sync.RWMutex
		ids map[string]bool
	}

	// Serialize all wacli commands
	storeLock sync.Mutex
)

type Config struct {
	WebhookURL    string
	WebhookSecret string
	APIToken      string
	WacliStore    string
	Port          string
}

func loadConfig() Config {
	c := Config{
		WebhookURL:    os.Getenv("WEBHOOK_URL"),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
		APIToken:      os.Getenv("API_TOKEN"),
		WacliStore:    os.Getenv("WACLI_STORE"),
		Port:          os.Getenv("PORT"),
	}
	if c.WacliStore == "" {
		c.WacliStore = "/data/wa-session"
	}
	if c.Port == "" {
		c.Port = "3100"
	}
	if c.WebhookURL == "" {
		log.Fatal("WEBHOOK_URL is required")
	}
	if c.APIToken == "" {
		log.Fatal("API_TOKEN is required")
	}
	return c
}

func main() {
	config = loadConfig()
	seenMessages.ids = make(map[string]bool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	// Initial sync
	log.Println("Running initial sync...")
	runSyncOnce()

	// Seed seen messages so we don't replay history
	seedSeenMessages()

	// Periodic sync + poll
	go syncLoop(ctx)
	go pollLoop(ctx)

	// Prune seen messages every hour
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pruneSeenMessages()
			}
		}
	}()

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/send", handleSend)
	mux.HandleFunc("/status", handleStatus)
	mux.HandleFunc("/health", handleHealth)

	server := &http.Server{
		Addr:         ":" + config.Port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("wa-gateway listening on :%s", config.Port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

// ═══════════════════════════════════════════════════════
// SYNC
// ═══════════════════════════════════════════════════════

func runSyncOnce() {
	storeLock.Lock()
	defer storeLock.Unlock()

	cmd := exec.Command("wacli", "sync", "--once",
		"--store", config.WacliStore,
		"--idle-exit", "2s")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Printf("Sync failed: %v", err)
		syncStatus.mu.Lock()
		syncStatus.connected = false
		syncStatus.mu.Unlock()
		return
	}

	syncStatus.mu.Lock()
	syncStatus.connected = true
	syncStatus.lastSync = time.Now()
	syncStatus.mu.Unlock()
}

func syncLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runSyncOnce()
		}
	}
}

// ═══════════════════════════════════════════════════════
// POLL
// ═══════════════════════════════════════════════════════

func pollLoop(ctx context.Context) {
	time.Sleep(1500 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkNewMessages()
		}
	}
}

func checkNewMessages() {
	msgs := fetchRecentMessages(20)
	for _, msg := range msgs {
		seenMessages.mu.RLock()
		seen := seenMessages.ids[msg.MsgID]
		seenMessages.mu.RUnlock()
		if seen {
			continue
		}

		seenMessages.mu.Lock()
		seenMessages.ids[msg.MsgID] = true
		seenMessages.mu.Unlock()

		go processMessage(msg)
	}
}

func fetchRecentMessages(limit int) []WacliMessage {
	storeLock.Lock()
	defer storeLock.Unlock()

	cmd := exec.Command("wacli", "messages", "list",
		"--json",
		"--limit", fmt.Sprintf("%d", limit),
		"--store", config.WacliStore)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var resp WacliMessagesResponse
	if err := json.Unmarshal(output, &resp); err != nil {
		return nil
	}
	if !resp.Success {
		return nil
	}
	return resp.Data.Messages
}

// ═══════════════════════════════════════════════════════
// SEEN MESSAGES
// ═══════════════════════════════════════════════════════

func seedSeenMessages() {
	msgs := fetchRecentMessages(100)
	seenMessages.mu.Lock()
	for _, msg := range msgs {
		seenMessages.ids[msg.MsgID] = true
	}
	seenMessages.mu.Unlock()
	log.Printf("Seeded %d existing messages as seen", len(msgs))
}

func pruneSeenMessages() {
	seenMessages.mu.Lock()
	if len(seenMessages.ids) > 10000 {
		seenMessages.ids = make(map[string]bool)
		log.Println("Pruned seen messages cache")
	}
	seenMessages.mu.Unlock()
}

// ═══════════════════════════════════════════════════════
// JID → from identifier
// ═══════════════════════════════════════════════════════

// normalizeJID converts @s.whatsapp.net JIDs to E.164 phone numbers.
// @lid JIDs pass through as-is (WhatsApp's newer privacy format —
// no phone number embedded). OtterClawd stores and matches on whatever
// identifier is returned.
func normalizeJID(jid string) string {
	if strings.HasSuffix(jid, "@s.whatsapp.net") {
		phone := strings.TrimSuffix(jid, "@s.whatsapp.net")
		phone = strings.Split(phone, ":")[0] // strip device suffix
		if phone != "" && phone != "0" && !strings.HasPrefix(phone, "+") {
			phone = "+" + phone
		}
		return phone
	}
	// @lid, @g.us, etc. → pass through as-is
	return jid
}

// ═══════════════════════════════════════════════════════
// PROCESS MESSAGE → WEBHOOK
// ═══════════════════════════════════════════════════════

func processMessage(msg WacliMessage) {
	if msg.FromMe {
		return
	}

	text := msg.Text
	if text == "" {
		text = msg.DisplayText
	}
	if text == "" {
		return
	}

	chatType := "dm"
	groupID := ""
	if strings.HasSuffix(msg.ChatJID, "@g.us") {
		chatType = "group"
		groupID = msg.ChatJID
	}

	from := normalizeJID(msg.SenderJID)

	payload := WebhookPayload{
		From:      from,
		Text:      text,
		PushName:  msg.ChatName,
		ChatType:  chatType,
		GroupID:   groupID,
		MessageID: msg.MsgID,
		Timestamp: msg.Timestamp,
	}

	log.Printf("New message from %s (%s): %s", from, msg.ChatName, truncate(text, 50))

	syncStatus.mu.Lock()
	syncStatus.lastMsg = time.Now()
	syncStatus.mu.Unlock()

	postToWebhook(payload)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func postToWebhook(payload WebhookPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal webhook payload: %v", err)
		return
	}

	req, err := http.NewRequest("POST", config.WebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("Failed to create webhook request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if config.WebhookSecret != "" {
		req.Header.Set("X-Webhook-Secret", config.WebhookSecret)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Webhook POST failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("Webhook returned %d: %s", resp.StatusCode, string(respBody))
	} else {
		log.Printf("Webhook delivered: %s → %d", payload.MessageID, resp.StatusCode)
	}
}

// ═══════════════════════════════════════════════════════
// OUTBOUND: REST → wacli send
// ═══════════════════════════════════════════════════════

func handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, SendResponse{Error: "Method not allowed"})
		return
	}

	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+config.APIToken {
		writeJSON(w, http.StatusUnauthorized, SendResponse{Error: "Unauthorized"})
		return
	}

	var req SendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, SendResponse{Error: "Invalid JSON"})
		return
	}

	if req.To == "" || req.Text == "" {
		writeJSON(w, http.StatusBadRequest, SendResponse{Error: "Missing 'to' or 'text'"})
		return
	}

	// Try to acquire lock with timeout — don't hang forever waiting for sync
	locked := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if storeLock.TryLock() {
			locked = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !locked {
		log.Println("Send timed out waiting for store lock")
		writeJSON(w, http.StatusServiceUnavailable, SendResponse{
			Error: "Gateway busy — try again in a few seconds",
		})
		return
	}

	cmd := exec.Command("wacli", "send", "text",
		"--to", req.To,
		"--message", req.Text,
		"--store", config.WacliStore,
		"--json")
	output, err := cmd.CombinedOutput()
	storeLock.Unlock()

	if err != nil {
		log.Printf("wacli send failed: %v — output: %s", err, string(output))
		writeJSON(w, http.StatusInternalServerError, SendResponse{
			Error: fmt.Sprintf("Send failed: %v", err),
		})
		return
	}

	var result map[string]interface{}
	messageID := ""
	if err := json.Unmarshal(output, &result); err == nil {
		for _, key := range []string{"MsgID", "MessageID", "msg_id", "messageId"} {
			if id, ok := result[key].(string); ok && id != "" {
				messageID = id
				break
			}
		}
		if messageID == "" {
			if data, ok := result["data"].(map[string]interface{}); ok {
				for _, key := range []string{"MsgID", "MessageID", "msg_id"} {
					if id, ok := data[key].(string); ok && id != "" {
						messageID = id
						break
					}
				}
			}
		}
	}

	log.Printf("Sent message to %s: %s", req.To, truncate(req.Text, 50))
	writeJSON(w, http.StatusOK, SendResponse{
		Success:   true,
		MessageID: messageID,
	})
}

// ═══════════════════════════════════════════════════════
// HEALTH / STATUS
// ═══════════════════════════════════════════════════════

func handleStatus(w http.ResponseWriter, r *http.Request) {
	syncStatus.mu.RLock()
	connected := syncStatus.connected
	lastMsg := syncStatus.lastMsg
	lastSync := syncStatus.lastSync
	syncStatus.mu.RUnlock()

	seenMessages.mu.RLock()
	seenCount := len(seenMessages.ids)
	seenMessages.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"connected":    connected,
		"lastMessage":  fmtTime(lastMsg),
		"lastSync":     fmtTime(lastSync),
		"seenMessages": seenCount,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	syncStatus.mu.RLock()
	connected := syncStatus.connected
	syncStatus.mu.RUnlock()

	if !connected {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("not connected"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
