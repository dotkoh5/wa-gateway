package main

import (
	"bufio"
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

// Payload we POST to OtterClawd
type WebhookPayload struct {
	From      string `json:"from"`
	Text      string `json:"text"`
	PushName  string `json:"pushName,omitempty"`
	ChatType  string `json:"chatType"`
	GroupID   string `json:"groupId,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	ReplyTo   string `json:"replyTo,omitempty"`
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
	config     Config
	syncStatus struct {
		mu        sync.RWMutex
		connected bool
		lastMsg   time.Time
	}
	// JID cache: maps @lid JIDs to phone numbers via wacli chats list
	jidCache struct {
		mu      sync.RWMutex
		mapping map[string]string // "120980773056584@lid" → "+6591234567"
		updated time.Time
	}
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
	jidCache.mapping = make(map[string]string)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	// Build initial JID cache
	refreshJIDCache()

	// Start wacli sync in background
	go runSync(ctx)

	// Periodically refresh JID cache (every 10 min)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshJIDCache()
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
// JID RESOLUTION: @lid → phone number
// ═══════════════════════════════════════════════════════

// WacliChat represents a chat entry from `wacli chats list --json`
type WacliChat struct {
	JID   string `json:"JID"`
	Name  string `json:"Name"`
	Phone string `json:"Phone"` // may or may not exist
}

func refreshJIDCache() {
	cmd := exec.Command("wacli", "chats", "list", "--json", "--store", config.WacliStore)
	output, err := cmd.Output()
	if err != nil {
		log.Printf("Failed to refresh JID cache: %v", err)
		return
	}

	var chats []WacliChat
	if err := json.Unmarshal(output, &chats); err != nil {
		// Try parsing as JSON lines
		scanner := bufio.NewScanner(bytes.NewReader(output))
		for scanner.Scan() {
			var chat WacliChat
			if json.Unmarshal(scanner.Bytes(), &chat) == nil {
				chats = append(chats, chat)
			}
		}
	}

	jidCache.mu.Lock()
	defer jidCache.mu.Unlock()

	for _, chat := range chats {
		// If JID is @s.whatsapp.net, the number prefix IS the phone
		if strings.HasSuffix(chat.JID, "@s.whatsapp.net") {
			phone := strings.TrimSuffix(chat.JID, "@s.whatsapp.net")
			phone = strings.Split(phone, ":")[0]
			if !strings.HasPrefix(phone, "+") {
				phone = "+" + phone
			}
			// Map both the @s JID and any @lid that maps to same contact
			jidCache.mapping[chat.JID] = phone
		}
		// If chat has a Phone field, use it directly
		if chat.Phone != "" {
			phone := chat.Phone
			if !strings.HasPrefix(phone, "+") {
				phone = "+" + phone
			}
			jidCache.mapping[chat.JID] = phone
		}
	}

	jidCache.updated = time.Now()
	log.Printf("JID cache refreshed: %d entries", len(jidCache.mapping))
}

// resolveJID converts any JID format to a phone number or returns the raw JID
func resolveJID(jid string) string {
	// @s.whatsapp.net → direct phone extraction
	if strings.HasSuffix(jid, "@s.whatsapp.net") {
		phone := strings.TrimSuffix(jid, "@s.whatsapp.net")
		phone = strings.Split(phone, ":")[0]
		if !strings.HasPrefix(phone, "+") {
			phone = "+" + phone
		}
		return phone
	}

	// @g.us → group, return as-is
	if strings.HasSuffix(jid, "@g.us") {
		return jid
	}

	// @lid → check cache
	jidCache.mu.RLock()
	phone, ok := jidCache.mapping[jid]
	jidCache.mu.RUnlock()
	if ok {
		return phone
	}

	// Cache miss — try a synchronous refresh
	log.Printf("JID cache miss for %s, refreshing...", jid)
	refreshJIDCache()

	jidCache.mu.RLock()
	phone, ok = jidCache.mapping[jid]
	jidCache.mu.RUnlock()
	if ok {
		return phone
	}

	// Still unresolved — pass the raw JID through
	// OtterClawd's normalizer will need to handle or ignore it
	log.Printf("WARNING: Could not resolve JID %s to phone number", jid)
	return jid
}

// ═══════════════════════════════════════════════════════
// INBOUND: wacli sync → parse → webhook
// ═══════════════════════════════════════════════════════

func runSync(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Println("Starting wacli sync...")
		cmd := exec.CommandContext(ctx, "wacli", "sync", "--follow", "--json",
			"--store", config.WacliStore)
		cmd.Stderr = os.Stderr

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("Failed to create stdout pipe: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if err := cmd.Start(); err != nil {
			log.Printf("Failed to start wacli sync: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		syncStatus.mu.Lock()
		syncStatus.connected = true
		syncStatus.mu.Unlock()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 256*1024), 256*1024) // 256KB buffer

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			// Copy line since scanner reuses the buffer
			msg := make([]byte, len(line))
			copy(msg, line)
			go processMessage(msg)
		}

		syncStatus.mu.Lock()
		syncStatus.connected = false
		syncStatus.mu.Unlock()

		if err := cmd.Wait(); err != nil {
			log.Printf("wacli sync exited: %v", err)
		}

		log.Println("wacli sync ended, restarting in 5s...")
		time.Sleep(5 * time.Second)
	}
}

func processMessage(raw []byte) {
	var msg WacliMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		log.Printf("Failed to parse wacli message: %v — raw: %s", err, string(raw))
		return
	}

	// Skip our own outbound messages
	if msg.FromMe {
		return
	}

	// Skip non-text messages for now
	text := msg.Text
	if text == "" {
		text = msg.DisplayText
	}
	if text == "" {
		return
	}

	// Determine chat type
	chatType := "dm"
	groupID := ""

	if strings.HasSuffix(msg.ChatJID, "@g.us") {
		chatType = "group"
		groupID = msg.ChatJID
	}

	// Resolve sender JID to phone number
	from := resolveJID(msg.SenderJID)

	payload := WebhookPayload{
		From:      from,
		Text:      text,
		PushName:  msg.ChatName, // wacli gives ChatName, not PushName
		ChatType:  chatType,
		GroupID:   groupID,
		MessageID: msg.MsgID,
		Timestamp: msg.Timestamp, // Already RFC3339
	}

	syncStatus.mu.Lock()
	syncStatus.lastMsg = time.Now()
	syncStatus.mu.Unlock()

	postToWebhook(payload)
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

	// Verify bearer token
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

	// Execute wacli send
	cmd := exec.Command("wacli", "send", "text",
		"--to", req.To,
		"--message", req.Text,
		"--store", config.WacliStore,
		"--json")

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("wacli send failed: %v — output: %s", err, string(output))
		writeJSON(w, http.StatusInternalServerError, SendResponse{
			Error: fmt.Sprintf("Send failed: %v", err),
		})
		return
	}

	// Try to extract messageId from wacli JSON output
	var result map[string]interface{}
	messageID := ""
	if err := json.Unmarshal(output, &result); err == nil {
		// Try common field names (PascalCase per wacli convention)
		for _, key := range []string{"MsgID", "MessageID", "msg_id", "messageId"} {
			if id, ok := result[key].(string); ok && id != "" {
				messageID = id
				break
			}
		}
		// Try nested data object
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
	syncStatus.mu.RUnlock()

	jidCache.mu.RLock()
	cacheSize := len(jidCache.mapping)
	cacheUpdated := jidCache.updated
	jidCache.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"connected": connected,
		"lastMessage": func() string {
			if lastMsg.IsZero() {
				return ""
			}
			return lastMsg.UTC().Format(time.RFC3339)
		}(),
		"jidCache": map[string]interface{}{
			"entries": cacheSize,
			"updated": func() string {
				if cacheUpdated.IsZero() {
					return ""
				}
				return cacheUpdated.UTC().Format(time.RFC3339)
			}(),
		},
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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
