package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/aigri/whatsapp-bot/internal/cache"
	"github.com/aigri/whatsapp-bot/internal/model"
	"go.uber.org/zap"
)

// WebhookHandler processes incoming WAHA webhook events and dispatches
// them to the worker pool for asynchronous processing.
type WebhookHandler struct {
	jobs        chan<- model.MessageJob
	cache       *cache.Client
	botNumber   string // our bot phone number (e.g. "628xxx")
	dedupeWindow time.Duration
	log         *zap.Logger
}

// NewWebhookHandler constructs a WebhookHandler.
func NewWebhookHandler(
	jobs chan<- model.MessageJob,
	cache *cache.Client,
	botNumber string,
	dedupeWindow time.Duration,
	log *zap.Logger,
) *WebhookHandler {
	return &WebhookHandler{
		jobs:        jobs,
		cache:       cache,
		botNumber:   botNumber,
		dedupeWindow: dedupeWindow,
		log:         log,
	}
}

// Handle is the HTTP handler for POST /webhook.
// It MUST always return 200 OK so WAHA does not retry.
func (h *WebhookHandler) Handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var event model.WAHAEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		h.log.Warn("webhook: failed to decode payload", zap.Error(err))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","note":"parse_error"}`)) //nolint:errcheck
		return
	}

	// Only process incoming text messages
	if event.Event != "message" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
		return
	}

	msg := event.Payload
	if msg == nil {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","note":"no_payload"}`)) //nolint:errcheck
		return
	}
	m := *msg // dereference once; helpers use value type

	// Capture bot's own linked device ID whenever we see a fromMe message
	if m.FromMe {
		h.captureBotDeviceID(r.Context(), m)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","note":"from_me_skipped"}`)) //nolint:errcheck
		return
	}

	// Extract message text
	body := extractBody(m)
	if body == "" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","note":"no_text"}`)) //nolint:errcheck
		return
	}

	// Determine sending phone and whether this comes from a group
	chatID := extractChatID(m)
	phone := extractSenderPhone(m, event.Engine)
	isGroup := strings.HasSuffix(chatID, "@g.us")

	// In groups: only respond when bot is explicitly mentioned
	if isGroup {
		botLinkedID, _ := h.cache.GetBotLinkedDeviceID(r.Context())
		if !isBotMentioned(m, h.botNumber, botLinkedID) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok","note":"not_mentioned"}`)) //nolint:errcheck
			return
		}
		body = cleanMentionText(body, h.botNumber)
	}

	// Deduplicate: skip if already processed within dedupeWindow
	msgID := extractMessageID(m)
	if msgID != "" {
		first, err := h.cache.MarkProcessed(r.Context(), msgID, h.dedupeWindow)
		if err == nil && !first {
			h.log.Debug("duplicate message skipped", zap.String("id", msgID))
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok","note":"duplicate"}`)) //nolint:errcheck
			return
		}
	}

	job := model.MessageJob{
		MessageID:   msgID,
		PhoneNumber: phone,
		ChatID:      chatID,
		MessageBody: body,
		IsGroup:     isGroup,
		DisplayName: extractDisplayName(m),
		ReceivedAt:  time.Now(),
	}

	// Non-blocking dispatch to worker pool
	select {
	case h.jobs <- job:
		h.log.Info("job queued",
			zap.String("phone", phone),
			zap.String("chat_id", chatID),
			zap.Bool("group", isGroup),
		)
	default:
		h.log.Warn("job queue full, message dropped",
			zap.String("phone", phone),
			zap.String("msg_id", msgID),
		)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}

// ---- Bot mention detection (6 methods, ported from PHP) --------------------

// isBotMentioned checks whether the bot was addressed in a group message
// via any of the 6 detection strategies.
func isBotMentioned(msg model.WAHAMessage, botNumber, botLinkedID string) bool {
	botClean := cleanPhone(botNumber)

	// 1. Check explicit to-field
	if to := msg.To; to != "" {
		if cleanPhone(to) == botClean {
			return true
		}
	}

	// 2. Check bot linked device ID (fromMe on group == bot's own device)
	if botLinkedID != "" {
		if cleanPhone(botLinkedID) == botClean {
			return true
		}
	}

	// 3. mentionedIds array
	for _, id := range msg.MentionedIDs {
		if cleanPhone(id) == botClean {
			return true
		}
	}

	// 4. @ mention in body text
	body := extractBody(msg)
	if strings.Contains(body, "@"+botClean) || strings.Contains(body, "@"+botNumber) {
		return true
	}

	// 5. Trigger words
	bodyLower := strings.ToLower(strings.TrimSpace(body))
	for _, trigger := range []string{"@bot", "bot,", "bot:", "bot!", "aigri", "@aigri"} {
		if strings.HasPrefix(bodyLower, trigger) || strings.Contains(bodyLower, trigger) {
			return true
		}
	}

	// 6. Quoted reply from bot
	if msg.QuotedMsg != nil && msg.QuotedMsg.FromMe {
		return true
	}

	return false
}

// ---- Payload extraction helpers -------------------------------------------

// extractBody returns the text body from the message.
func extractBody(msg model.WAHAMessage) string {
	if msg.Body != "" {
		return msg.Body
	}
	if msg.Caption != "" {
		return msg.Caption
	}
	if msg.ExtendedText != nil && msg.ExtendedText.Text != "" {
		return msg.ExtendedText.Text
	}
	return ""
}

// extractChatID returns the chat room ID from the message.
func extractChatID(msg model.WAHAMessage) string {
	if msg.ChatID != "" {
		return msg.ChatID
	}
	if msg.From != "" {
		return msg.From
	}
	if msg.InternalData != nil {
		if id := msg.InternalData.RemoteJid; id != "" {
			return id
		}
	}
	return ""
}

// extractSenderPhone returns the sending phone number, normalised.
// For NOWEB engine it uses remoteJidAlt; for WEBJS via remoteJid.
func extractSenderPhone(msg model.WAHAMessage, engine string) string {
	if msg.InternalData != nil {
		if engine == "NOWEB" && msg.InternalData.RemoteJidAlt != "" {
			return cleanPhone(msg.InternalData.RemoteJidAlt)
		}
		if msg.InternalData.RemoteJid != "" {
			return cleanPhone(msg.InternalData.RemoteJid)
		}
	}
	// Fallback to flat fields
	if msg.Sender != "" {
		return cleanPhone(msg.Sender)
	}
	if msg.From != "" {
		from := msg.From
		// In group messages "from" is "groupid@g.us/senderID"
		if idx := strings.Index(from, "/"); idx != -1 {
			from = from[idx+1:]
		}
		return cleanPhone(from)
	}
	return ""
}

// extractMessageID returns a unique message ID for deduplication.
func extractMessageID(msg model.WAHAMessage) string {
	if msg.ID != "" {
		return msg.ID
	}
	if msg.InternalData != nil {
		return msg.InternalData.ID
	}
	return ""
}

// extractDisplayName returns the user's display / push name.
func extractDisplayName(msg model.WAHAMessage) string {
	if msg.NotifyName != "" {
		return msg.NotifyName
	}
	if msg.PushName != "" {
		return msg.PushName
	}
	return ""
}

// cleanMentionText strips bot @-mention from message body in group context.
func cleanMentionText(body, botNumber string) string {
	cleaned := strings.ReplaceAll(body, "@"+botNumber, "")
	cleaned = strings.ReplaceAll(cleaned, "@"+cleanPhone(botNumber), "")
	return strings.TrimSpace(cleaned)
}

// cleanPhone strips non-numeric characters and @-domain suffixes.
func cleanPhone(raw string) string {
	if idx := strings.Index(raw, "@"); idx != -1 {
		raw = raw[:idx]
	}
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, c := range raw {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// captureBotDeviceID reads the bot's own linked device ID from a fromMe event
// (NOWEB reports "lid" as the device identifier) and caches it for later use.
func (h *WebhookHandler) captureBotDeviceID(ctx context.Context, msg model.WAHAMessage) {
	if msg.Me == nil {
		return
	}
	lid := msg.Me.LID
	if lid == "" {
		lid = msg.Me.ID
	}
	if lid == "" {
		return
	}
	if err := h.cache.SetBotLinkedDeviceID(ctx, lid); err != nil {
		h.log.Warn("SetBotLinkedDeviceID failed", zap.Error(err))
	}
}
