package wa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

type Bridge struct {
	client        *whatsmeow.Client
	mu            sync.RWMutex
	state         string // disconnected, connecting, open
	qrDataURL     string
	pairingCode   string
	webhookURL    string
	webhookSecret string
}

func NewBridge() *Bridge {
	return &Bridge{
		state:         "disconnected",
		webhookURL:    os.Getenv("WEBHOOK_URL"),
		webhookSecret: os.Getenv("WEBHOOK_SECRET"),
	}
}

func (b *Bridge) State() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state
}

func (b *Bridge) QR() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.qrDataURL
}

func (b *Bridge) PairingCode() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.pairingCode
}

func (b *Bridge) Connect() {
	dbLog := waLog.Noop
	container, err := sqlstore.New(context.Background(), "sqlite", "file:wa_store.db?_pragma=foreign_keys(1)", dbLog)
	if err != nil {
		log.Fatal("[whatsapp] Failed to init store:", err)
	}

	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Fatal("[whatsapp] Failed to get device:", err)
	}

	clientLog := waLog.Stdout("Client", "WARN", true)
	b.client = whatsmeow.NewClient(device, clientLog)
	b.client.AddEventHandler(b.eventHandler)

	if b.client.Store.ID == nil {
		// Not logged in — need QR or pairing code
		pairingPhone := os.Getenv("PAIRING_PHONE_NUMBER")
		if pairingPhone != "" {
			b.connectWithPairingCode(pairingPhone)
		} else {
			b.connectWithQR()
		}
	} else {
		// Already logged in
		err = b.client.Connect()
		if err != nil {
			log.Fatal("[whatsapp] Failed to connect:", err)
		}
		b.mu.Lock()
		b.state = "open"
		b.mu.Unlock()
		log.Println("[whatsapp] Connected (existing session)")
	}
}

func (b *Bridge) connectWithQR() {
	qrChan, _ := b.client.GetQRChannel(context.Background())
	err := b.client.Connect()
	if err != nil {
		log.Fatal("[whatsapp] Failed to connect:", err)
	}

	for evt := range qrChan {
		switch evt.Event {
		case "code":
			b.mu.Lock()
			b.state = "connecting"
			// Generate QR as data URL
			png, err := qrcode.Encode(evt.Code, qrcode.Medium, 300)
			if err == nil {
				b.qrDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
			}
			b.mu.Unlock()
			// Also print to terminal
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			log.Println("[whatsapp] QR code ready — scan from the web page")
		case "login":
			b.mu.Lock()
			b.state = "open"
			b.qrDataURL = ""
			b.mu.Unlock()
			log.Println("[whatsapp] Connected via QR")
		case "timeout":
			log.Println("[whatsapp] QR timeout, reconnecting...")
			b.mu.Lock()
			b.state = "disconnected"
			b.qrDataURL = ""
			b.mu.Unlock()
			go b.Connect()
			return
		}
	}
}

func (b *Bridge) connectWithPairingCode(phone string) {
	err := b.client.Connect()
	if err != nil {
		log.Fatal("[whatsapp] Failed to connect:", err)
	}

	b.mu.Lock()
	b.state = "connecting"
	b.mu.Unlock()

	// Wait a moment for the connection to establish
	time.Sleep(2 * time.Second)

	code, err := b.client.PairPhone(context.Background(), phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		log.Printf("[whatsapp] Failed to get pairing code: %v", err)
		// Fallback to QR
		b.client.Disconnect()
		b.connectWithQR()
		return
	}

	b.mu.Lock()
	b.pairingCode = code
	b.mu.Unlock()
	log.Printf("[whatsapp] Pairing code: %s — enter in WhatsApp > Linked Devices > Link with phone number", code)
}

func (b *Bridge) eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		b.mu.Lock()
		b.state = "open"
		b.qrDataURL = ""
		b.pairingCode = ""
		b.mu.Unlock()
		log.Println("[whatsapp] Connected")

	case *events.Disconnected:
		b.mu.Lock()
		b.state = "disconnected"
		b.mu.Unlock()
		log.Println("[whatsapp] Disconnected")

	case *events.LoggedOut:
		b.mu.Lock()
		b.state = "disconnected"
		b.mu.Unlock()
		log.Println("[whatsapp] Logged out — delete wa_store.db and restart")

	case *events.Message:
		if v.Info.IsFromMe {
			return
		}

		// Prefer phone number (PN) over LID
		phone := v.Info.Sender.User
		senderServer := v.Info.Sender.Server
		chatUser := v.Info.Chat.User
		chatServer := v.Info.Chat.Server
		isGroup := v.Info.IsGroup
		log.Printf("[whatsapp] Message from sender=%s@%s chat=%s@%s pushName=%s group=%v", phone, senderServer, chatUser, chatServer, v.Info.PushName, isGroup)

		// Resolve LID to phone number using SenderAlt (PN JID)
		if senderServer == "lid" {
			if v.Info.MessageSource.SenderAlt.User != "" && v.Info.MessageSource.SenderAlt.Server != "lid" {
				phone = v.Info.MessageSource.SenderAlt.User
			}
		}
		name := v.Info.PushName
		text := ""
		msgType := "text"
		caption := ""
		var mentionedJids []string

		if v.Message.GetConversation() != "" {
			text = v.Message.GetConversation()
		} else if v.Message.GetExtendedTextMessage() != nil {
			text = v.Message.GetExtendedTextMessage().GetText()
			// Extract mentioned JIDs
			if ci := v.Message.GetExtendedTextMessage().GetContextInfo(); ci != nil {
				for _, jid := range ci.GetMentionedJID() {
					mentionedJids = append(mentionedJids, jid)
				}
			}
		} else if v.Message.GetImageMessage() != nil {
			msgType = "image"
			caption = v.Message.GetImageMessage().GetCaption()
		} else if v.Message.GetVideoMessage() != nil {
			msgType = "video"
			caption = v.Message.GetVideoMessage().GetCaption()
		} else if v.Message.GetAudioMessage() != nil {
			msgType = "audio"
		} else if v.Message.GetDocumentMessage() != nil {
			msgType = "document"
			caption = v.Message.GetDocumentMessage().GetCaption()
		}

		if text == "" && caption != "" {
			text = caption
		}
		if text == "" {
			text = fmt.Sprintf("[%s]", msgType)
		}

		botJID := ""
		if b.client != nil && b.client.Store != nil && b.client.Store.ID != nil {
			botJID = b.client.Store.ID.User
		}

		b.sendWebhook(map[string]any{
			"event": "message.received",
			"contact": map[string]any{
				"phoneNumber": phone,
				"name":        name,
			},
			"message": map[string]any{
				"id":            v.Info.ID,
				"type":          msgType,
				"text":          text,
				"caption":       caption,
				"from":          phone,
				"timestamp":     v.Info.Timestamp.Unix(),
				"mentionedJids": mentionedJids,
			},
			"group": map[string]any{
				"isGroup": isGroup,
				"id":      chatUser,
			},
			"bot": map[string]any{
				"jid": botJID,
			},
		})
	}
}

func (b *Bridge) sendWebhook(data map[string]any) {
	if b.webhookURL == "" {
		return
	}
	body, _ := json.Marshal(data)
	req, err := http.NewRequest("POST", b.webhookURL, strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if b.webhookSecret != "" {
		req.Header.Set("X-Webhook-Secret", b.webhookSecret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[webhook] Failed: %v", err)
		return
	}
	resp.Body.Close()
}

func (b *Bridge) SendText(to, message string, replyTo string) (string, error) {
	if b.client == nil || b.state != "open" {
		return "", fmt.Errorf("not connected")
	}
	jid := toJID(to)
	msg := &waE2E.Message{
		Conversation: proto.String(message),
	}
	if replyTo != "" {
		msg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(message),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID: proto.String(replyTo),
				},
			},
		}
	}
	resp, err := b.client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (b *Bridge) SendMedia(to, url, caption, mediaType string) (string, error) {
	if b.client == nil || b.state != "open" {
		return "", fmt.Errorf("not connected")
	}

	// Download media
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to download media: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read media: %w", err)
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}

	jid := toJID(to)
	uploaded, err := b.client.Upload(context.Background(), data, whatsmeow.MediaImage)
	if err != nil {
		return "", fmt.Errorf("failed to upload: %w", err)
	}

	var msg *waE2E.Message
	switch mediaType {
	case "video":
		uploaded2, _ := b.client.Upload(context.Background(), data, whatsmeow.MediaVideo)
		msg = &waE2E.Message{
			VideoMessage: &waE2E.VideoMessage{
				URL:           &uploaded2.URL,
				DirectPath:    &uploaded2.DirectPath,
				MediaKey:      uploaded2.MediaKey,
				FileEncSHA256: uploaded2.FileEncSHA256,
				FileSHA256:    uploaded2.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
				Mimetype:      &mime,
				Caption:       proto.String(caption),
			},
		}
	case "document":
		uploaded2, _ := b.client.Upload(context.Background(), data, whatsmeow.MediaDocument)
		msg = &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				URL:           &uploaded2.URL,
				DirectPath:    &uploaded2.DirectPath,
				MediaKey:      uploaded2.MediaKey,
				FileEncSHA256: uploaded2.FileEncSHA256,
				FileSHA256:    uploaded2.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
				Mimetype:      &mime,
				Caption:       proto.String(caption),
				Title:         proto.String(caption),
			},
		}
	case "audio":
		uploaded2, _ := b.client.Upload(context.Background(), data, whatsmeow.MediaAudio)
		msg = &waE2E.Message{
			AudioMessage: &waE2E.AudioMessage{
				URL:           &uploaded2.URL,
				DirectPath:    &uploaded2.DirectPath,
				MediaKey:      uploaded2.MediaKey,
				FileEncSHA256: uploaded2.FileEncSHA256,
				FileSHA256:    uploaded2.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
				Mimetype:      &mime,
			},
		}
	default: // image
		msg = &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				URL:           &uploaded.URL,
				DirectPath:    &uploaded.DirectPath,
				MediaKey:      uploaded.MediaKey,
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
				Mimetype:      &mime,
				Caption:       proto.String(caption),
			},
		}
	}

	sendResp, err := b.client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		return "", err
	}
	return sendResp.ID, nil
}

func toJID(phone string) types.JID {
	phone = strings.ReplaceAll(phone, "+", "")
	phone = strings.ReplaceAll(phone, " ", "")
	phone = strings.ReplaceAll(phone, "-", "")
	// Support full JID format (e.g. "120363418234909480@g.us")
	if parts := strings.SplitN(phone, "@", 2); len(parts) == 2 {
		return types.NewJID(parts[0], parts[1])
	}
	return types.NewJID(phone, types.DefaultUserServer)
}
