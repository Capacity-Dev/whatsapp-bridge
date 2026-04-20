package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"whatsapp-bridge/wa"
)

func main() {
	loadEnv(".env")
	apiPathSecret := os.Getenv("API_PATH_SECRET")
	apiKey := os.Getenv("API_KEY")
	qrSecret := os.Getenv("QR_ACCESS_SECRET")
	port := os.Getenv("PORT")
	if port == "" {
		port = "3567"
	}
	if len(apiPathSecret) < 16 {
		log.Fatal("API_PATH_SECRET must be at least 16 characters")
	}
	if apiKey == "" {
		log.Fatal("API_KEY is required")
	}
	if len(qrSecret) < 32 {
		log.Fatal("QR_ACCESS_SECRET must be at least 32 characters")
	}

	bridge := wa.NewBridge()
	go bridge.Connect()

	base := "/api-" + apiPathSecret
	mux := http.NewServeMux()

	// Health (under secret path)
	mux.HandleFunc("GET "+base+"/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 200, map[string]any{"status": "ok", "whatsapp": bridge.State()})
	})

	// QR page (secret-protected + brute force)
	qrLimiter := newRateLimiter(5, 15*time.Minute)
	mux.HandleFunc("GET "+base+"/qr", func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if !qrLimiter.allow(ip) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(429)
			fmt.Fprint(w, qrPage("Too many attempts. Try again later.", ""))
			return
		}
		secret := r.URL.Query().Get("secret")
		if subtle.ConstantTimeCompare([]byte(secret), []byte(qrSecret)) != 1 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(401)
			fmt.Fprint(w, qrPage("Access denied.", ""))
			return
		}

		w.Header().Set("Content-Type", "text/html")
		state := bridge.State()
		if state == "open" {
			fmt.Fprint(w, qrPage("", `<span class="status connected">✓ Connected</span><script>setTimeout(()=>location.reload(),10000)</script>`))
			return
		}
		qr := bridge.QR()
		pairingCode := bridge.PairingCode()
		if pairingCode != "" {
			fmt.Fprint(w, qrPage("", fmt.Sprintf(`<span class="status waiting">Enter Pairing Code</span><p style="margin-top:1rem">WhatsApp → Linked Devices → Link with phone number</p><div style="margin-top:1rem;font-size:2rem;font-weight:bold;letter-spacing:.3em;font-family:monospace">%s</div><script>setTimeout(()=>location.reload(),15000)</script>`, pairingCode)))
			return
		}
		if qr != "" {
			fmt.Fprint(w, qrPage("", fmt.Sprintf(`<span class="status waiting">Scan QR Code</span><p style="margin-top:1rem">WhatsApp → Linked Devices → Link a Device</p><img src="%s" style="margin-top:1rem"/><script>setTimeout(()=>location.reload(),20000)</script>`, qr)))
			return
		}
		fmt.Fprint(w, qrPage("", `<span class="status waiting">Connecting...</span><script>setTimeout(()=>location.reload(),3000)</script>`))
	})

	// API endpoints (API key protected)
	requireKey := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				key = r.URL.Query().Get("apiKey")
			}
			if subtle.ConstantTimeCompare([]byte(key), []byte(apiKey)) != 1 {
				jsonResp(w, 401, map[string]any{"error": "Unauthorized"})
				return
			}
			next(w, r)
		}
	}

	// POST /send — send text message
	mux.HandleFunc("POST "+base+"/send", requireKey(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			To      string `json:"to"`
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.To == "" || body.Message == "" {
			jsonResp(w, 400, map[string]any{"error": "to and message are required"})
			return
		}
		id, err := bridge.SendText(body.To, body.Message)
		if err != nil {
			jsonResp(w, 500, map[string]any{"error": err.Error()})
			return
		}
		jsonResp(w, 200, map[string]any{"success": true, "messageId": id})
	}))

	// POST /send-media — send media via URL
	mux.HandleFunc("POST "+base+"/send-media", requireKey(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			To      string `json:"to"`
			URL     string `json:"url"`
			Caption string `json:"caption"`
			Type    string `json:"type"` // image, video, document, audio
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.To == "" || body.URL == "" {
			jsonResp(w, 400, map[string]any{"error": "to and url are required"})
			return
		}
		if body.Type == "" {
			body.Type = "image"
		}
		id, err := bridge.SendMedia(body.To, body.URL, body.Caption, body.Type)
		if err != nil {
			jsonResp(w, 500, map[string]any{"error": err.Error()})
			return
		}
		jsonResp(w, 200, map[string]any{"success": true, "messageId": id})
	}))

	// GET /status
	mux.HandleFunc("GET "+base+"/status", requireKey(func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 200, map[string]any{"connected": bridge.State() == "open", "state": bridge.State()})
	}))

	// 404 for everything else
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})

	log.Printf("[server] Listening on :%s", port)
	log.Printf("[server] API base: %s", base)
	log.Printf("[server] QR page:  %s/qr?secret=<QR_ACCESS_SECRET>", base)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func jsonResp(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func qrPage(message, content string) string {
	body := content
	if body == "" {
		body = "<p>" + message + "</p>"
	}
	return `<!DOCTYPE html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>WhatsApp Bridge</title><style>*{margin:0;padding:0;box-sizing:border-box}body{font-family:system-ui,sans-serif;background:#0a0a0a;color:#fff;display:flex;align-items:center;justify-content:center;min-height:100vh}.card{background:#111;border:1px solid #222;border-radius:12px;padding:2rem;text-align:center;max-width:400px;width:100%}.card h1{font-size:1.2rem;margin-bottom:1rem}.card p{color:#888;font-size:.875rem;margin-bottom:1rem}.status{display:inline-block;padding:4px 12px;border-radius:999px;font-size:.75rem;font-weight:600}.connected{background:#22c55e20;color:#22c55e}.waiting{background:#eab30820;color:#eab308}</style></head><body><div class="card"><h1>WhatsApp Bridge</h1>` + body + `</div></body></html>`
}

// Simple in-memory rate limiter
type rateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	max      int
	window   time.Duration
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{attempts: make(map[string][]time.Time), max: max, window: window}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rl.window)
	var valid []time.Time
	for _, t := range rl.attempts[key] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= rl.max {
		rl.attempts[key] = valid
		return false
	}
	rl.attempts[key] = append(valid, now)
	return true
}

func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			key := strings.TrimSpace(line[:i])
			val := strings.TrimSpace(line[i+1:])
			val = strings.Trim(val, `"'`)
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
}
