// Package service implements the WhatsApp message routing logic.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aigri/whatsapp-bot/internal/cache"
	"github.com/aigri/whatsapp-bot/internal/client"
	"github.com/aigri/whatsapp-bot/internal/model"
	"github.com/aigri/whatsapp-bot/internal/repository"
	"go.uber.org/zap"
)

// RoutingService contains all logic to handle an incoming WhatsApp message
// and produce a reply via WAHA – equivalent to the PHP MessageRoutingService.
type RoutingService struct {
	db    *repository.DB
	cache *cache.Client
	waha  *client.WAHAClient
	rag   *client.RAGClient
	ai    *AIHelper
	log   *zap.Logger

	// Config knobs
	sessionTTL      time.Duration // default 5 min
	historyTTL      time.Duration // default 30 min
	dedupeWindow    time.Duration
	wahaAutoRestart bool
	wahaMaxErrors   int64
}

// NewRoutingService constructs a RoutingService.
func NewRoutingService(
	db *repository.DB,
	cache *cache.Client,
	waha *client.WAHAClient,
	rag *client.RAGClient,
	ai *AIHelper,
	dedupeWindow time.Duration,
	wahaAutoRestart bool,
	log *zap.Logger,
) *RoutingService {
	return &RoutingService{
		db:              db,
		cache:           cache,
		waha:            waha,
		rag:             rag,
		ai:              ai,
		log:             log,
		sessionTTL:      5 * time.Minute,
		historyTTL:      30 * time.Minute,
		dedupeWindow:    dedupeWindow,
		wahaAutoRestart: wahaAutoRestart,
		wahaMaxErrors:   5,
	}
}

// ProcessJob is the main entry point called by the worker pool.
// It runs the full routing pipeline for one incoming message job.
func (r *RoutingService) ProcessJob(ctx context.Context, job model.MessageJob) {
	start := time.Now()
	log := r.log.With(
		zap.String("phone", job.PhoneNumber),
		zap.String("chat_id", job.ChatID),
		zap.Bool("is_group", job.IsGroup),
	)

	defer func() {
		log.Info("job processed", zap.Duration("elapsed", time.Since(start)))
	}()

	// Priority 1: Identity question
	if r.ai.IsIdentityQuestion(ctx, job.MessageBody) {
		log.Info("identity question detected")
		intro := r.buildPersonalizedIntro(ctx, job)
		result := r.waha.SendText(ctx, job.ChatID, intro)
		r.handleSendResult(ctx, result, job.ChatID)
		r.appendHistory(ctx, job.ChatID, job.MessageBody, intro)
		return
	}

	// Priority 2: Simple greeting
	if r.ai.IsSimpleGreeting(job.MessageBody) {
		log.Info("simple greeting detected")
		reply, err := r.ai.QuickLLMResponse(ctx, job.MessageBody)
		if err != nil {
			log.Warn("quick llm failed", zap.Error(err))
			reply = "Halo! Ada yang bisa saya bantu? 😊"
		}
		result := r.waha.SendText(ctx, job.ChatID, reply)
		r.handleSendResult(ctx, result, job.ChatID)
		r.appendHistory(ctx, job.ChatID, job.MessageBody, reply)
		return
	}

	// Route through knowledge rooms
	routeResult := r.routeMessage(ctx, job)

	if routeResult.Message != "" {
		sendResult := r.waha.SendText(ctx, job.ChatID, routeResult.Message)
		r.handleSendResult(ctx, sendResult, job.ChatID)
		r.appendHistory(ctx, job.ChatID, job.MessageBody, routeResult.Message)
	}
}

// routeMessage is the room-based routing pipeline.
func (r *RoutingService) routeMessage(ctx context.Context, job model.MessageJob) model.RouteResult {
	lookupID := job.ChatID
	if !job.IsGroup {
		lookupID = job.PhoneNumber
	}
	variants := phoneVariants(lookupID)

	// STEP 0: Load chatbot links
	links, err := r.db.FindChatbotLinksByPhone(ctx, variants)
	if err != nil {
		r.log.Error("FindChatbotLinks failed", zap.Error(err))
		return model.RouteResult{Message: "❌ Terjadi kesalahan sistem. Mohon coba beberapa saat lagi."}
	}

	// Handle system commands (needs links for statistics)
	if cmd := r.handleSystemCommand(ctx, job, links, lookupID, variants); cmd != nil {
		return *cmd
	}

	if len(links) == 0 {
		if job.IsGroup {
			return model.RouteResult{
				Message:   fmt.Sprintf("❌ *Group ini belum memiliki akses.*\n\nHarap laporkan ID Group Anda `%s` ke Administrator AI.", job.ChatID),
				RouteType: "no_room_access",
			}
		}
		return model.RouteResult{
			Message:   fmt.Sprintf("❌ *Nomor ini belum memiliki akses.*\n\nHarap laporkan Nomor Anda `%s` ke Administrator AI.", job.PhoneNumber),
			RouteType: "no_room_access",
		}
	}

	// STEP 1.5: Load conversation context for query transformation
	sessionID := r.getOrCreateSessionID(ctx, lookupID, variants)
	waHistory, err := r.db.GetWhatsAppHistoryBySession(ctx, variants, sessionID, 10)
	if err != nil {
		r.log.Warn("GetWhatsAppHistory failed", zap.Error(err))
	}
	convContext := waHistoryToPairs(waHistory)

	// STEP 2: Transform query if short + context available
	query := job.MessageBody
	queryTransformed := false
	if len(convContext) > 0 && len(query) < 50 {
		newQ := r.ai.TransformQuery(ctx, query, convContext)
		if newQ != query {
			query = newQ
			queryTransformed = true
			r.log.Info("query transformed", zap.String("original", job.MessageBody), zap.String("transformed", query))

			// Validate clarity
			if ok, clarMsg := r.ai.ValidateQueryClarity(query); !ok {
				return model.RouteResult{Message: clarMsg, RouteType: "clarification_needed"}
			}
		}
	}

	// STEP 3: Select best room via LLM
	idx := r.ai.SelectBestRoom(ctx, query, links)
	selected := links[idx]
	if selected.Room == nil {
		return model.RouteResult{Message: "❌ Room tidak ditemukan. Hubungi admin.", RouteType: "room_not_found"}
	}
	room := selected.Room

	// Send agent confirmation
	queryShown := query
	if !queryTransformed {
		queryShown = job.MessageBody
	}
	confirmMsg := fmt.Sprintf("🤖 Saya akan menjawab pertanyaan Anda terkait *%s* dengan menggunakan data *%s*\n\n🔍 _Sedang mencari jawaban..._",
		queryShown, room.Name)
	r.waha.SendText(ctx, job.ChatID, confirmMsg) //nolint:errcheck - best-effort

	// Update last accessed
	_ = r.db.UpdateLastAccessed(ctx, selected.ID)

	// STEP 4: Get or create virtual user
	virtualUser, err := r.db.GetOrCreateVirtualUser(ctx, selected.Phone, selected.Name)
	if err != nil {
		r.log.Error("GetOrCreateVirtualUser failed", zap.Error(err))
		return model.RouteResult{Message: "❌ Terjadi kesalahan sistem.", RouteType: "error"}
	}

	// STEP 5: Conversation context for room
	threadID := fmt.Sprintf("chatbot_%d", selected.ID)
	prevConvs, err := r.db.GetConversationContext(ctx, room.ID, virtualUser.ID, threadID, 5)
	if err != nil {
		r.log.Warn("GetConversationContext failed", zap.Error(err))
	}

	// STEP 5.5: Detect topic change and warn user
	if len(prevConvs) > 0 {
		if changed := r.ai.DetectTopicChange(ctx, job.MessageBody, prevConvs); changed {
			hint := "💡 *Perhatian: Topik Berubah Terdeteksi!*\n\n" +
				"Untuk hasil lebih akurat, gunakan `/clear` untuk menghapus riwayat percakapan lama.\n\n" +
				"Atau saya akan tetap memproses pertanyaan Anda...\n\n⏱ _Memproses dalam 2 detik..._"
			r.waha.SendText(ctx, job.ChatID, hint) //nolint:errcheck
			time.Sleep(2 * time.Second)
		}
	}

	// STEP 6: Execute room query via RAG
	resources, err := r.db.FindActiveResourcesByRoom(ctx, room.ID)
	if err != nil {
		r.log.Error("FindActiveResources failed", zap.Error(err))
	}

	// Save user message to conversation history
	convID, err := r.db.InsertConversation(ctx, &model.ConversationHistory{
		RoomID:      room.ID,
		UserID:      virtualUser.ID,
		ThreadID:    threadID,
		Query:       job.MessageBody,
		LLMResponse: "",
	})
	if err != nil {
		r.log.Warn("InsertConversation failed", zap.Error(err))
	}

	var aiResponse string
	if len(resources) == 0 {
		aiResponse = fmt.Sprintf("📭 Room *%s* belum memiliki sumber data.\n\nSilakan hubungi admin untuk menambahkan data resource.", room.Name)
	} else {
		bestResource := resources[0] // first active resource (could be enhanced with LLM selection)
		execResult := r.rag.ExecuteResource(ctx, bestResource, query,
			selected.Name, selected.Phone, room.Name, prevConvs)

		if execResult.Success {
			aiResponse = execResult.Answer
			// Append sources
			if len(execResult.Sources) > 0 {
				aiResponse += "\n\n📚 *Sumber:*\n"
				for _, s := range execResult.Sources {
					name := s.Title
					if name == "" {
						name = s.Source
					}
					aiResponse += fmt.Sprintf("• %s\n", name)
				}
			}
			aiResponse += fmt.Sprintf("\n\n🤖 _Agent: %s_", room.Name)
			aiResponse += "\n\n💡 _Ingin ganti topik? Ketik_ `/clear` _untuk memulai percakapan baru._"
		} else {
			aiResponse = "❌ Maaf, terjadi kesalahan saat mengambil data.\n\n" + execResult.Error
		}
	}

	// STEP 7: Persist response
	if convID > 0 {
		_ = r.db.UpdateConversationResponse(ctx, convID, aiResponse)
	}
	// Persist to WhatsApp history
	_ = r.db.InsertWhatsAppHistory(ctx, &model.WhatsAppConversationHistory{
		PhoneNumber: selected.Phone,
		SessionID:   sessionID,
		UserMessage: job.MessageBody,
		BotResponse: aiResponse,
		RouteType:   "room",
		Kategori:    room.Name,
	})

	return model.RouteResult{
		Success:   true,
		Message:   aiResponse,
		Source:    "room",
		RouteType: "room",
		RoomID:    room.ID,
		RoomName:  room.Name,
		SessionID: sessionID,
	}
}

// handleSystemCommand processes commands like /clear, /rooms, /help, /stats.
// Returns nil if the message is not a command.
func (r *RoutingService) handleSystemCommand(
	ctx context.Context,
	job model.MessageJob,
	links []*model.ChatbotLink,
	lookupID string,
	variants []string,
) *model.RouteResult {
	msgLower := strings.ToLower(strings.TrimSpace(job.MessageBody))

	// ---- /rooms ----
	if isOneOf(msgLower, "!rooms", "!list", "/rooms", "/list", "daftar room", "list room") {
		if len(links) == 0 {
			return &model.RouteResult{
				Message:   fmt.Sprintf("ℹ️ *Anda belum memiliki akses.*\n\nHarap laporkan nomor Anda `%s` ke Administrator AI.", lookupID),
				RouteType: "command_rooms_empty",
			}
		}
		msg := "📚 *Daftar Knowledge Room Anda:*\n\n"
		for i, l := range links {
			if l.Room == nil {
				continue
			}
			msg += fmt.Sprintf("%d. *%s*\n", i+1, l.Room.Name)
			if l.Room.Description != "" {
				msg += fmt.Sprintf("   _%s_\n", l.Room.Description)
			}
			msg += "\n"
		}
		msg += "💡 _Ketik pertanyaan Anda dan sistem akan otomatis memilih room yang relevan._"
		return &model.RouteResult{Message: msg, RouteType: "list_rooms"}
	}

	// ---- /clear ----
	clearCmds := []string{
		"!clear", "/clear", "/reset", "/mulai baru", "/hapus riwayat",
		"clear history", "hapus riwayat", "reset chat",
		"mulai dari awal", "ganti topik", "/new", "/baru",
		"topik baru", "clear", "reset",
	}
	if isOneOf(msgLower, clearCmds...) {
		newSession := newSessionID(lookupID)
		oldSession := r.getOrCreateSessionID(ctx, lookupID, variants)
		oldCount, _ := r.db.CountSessionMessages(ctx, variants, oldSession)
		totalSessions, _ := r.db.CountDistinctSessions(ctx, variants)

		_ = r.cache.SetSession(ctx, lookupID, newSession, r.sessionTTL)
		r.log.Info("new session started",
			zap.String("old_session", oldSession),
			zap.String("new_session", newSession),
			zap.Int("old_msg_count", oldCount),
		)

		contextNote := "Anda"
		if job.IsGroup {
			contextNote = "group ini"
		}
		msg := fmt.Sprintf("🆕 *Percakapan baru telah dimulai!*\n\n"+
			"📊 *Detail Session:*\n"+
			"   • Session sebelumnya: %d pesan\n"+
			"   • Total riwayat session: %d\n"+
			"   • Status: Fresh start (tanpa konteks lama)\n\n"+
			"🔄 *Topik baru siap!*\n"+
			"Sekarang %s bisa bertanya tentang topik yang berbeda.\n\n"+
			"💡 *Tips berguna:*\n"+
			"• Ketik `/clear` kapan saja untuk mulai topik baru\n"+
			"• Ketik `/stats` untuk lihat statistik semua session\n"+
			"• Ketik `/help` untuk lihat semua perintah",
			oldCount, totalSessions+1, contextNote)
		return &model.RouteResult{Message: msg, RouteType: "new_session", SessionID: newSession}
	}

	// ---- /help ----
	if isOneOf(msgLower, "!help", "/help", "/bantuan", "help", "bantuan", "/commands", "/perintah") {
		msg := fmt.Sprintf("🤖 *Bantuan AIGRI Bot*\n\n"+
			"*Perintah yang tersedia:*\n"+
			"• `/rooms` - Lihat daftar room Anda\n"+
			"• `/clear` - Hapus riwayat percakapan\n"+
			"• `/reset` atau `/mulai baru` - Sama dengan clear\n"+
			"• `/help` - Tampilkan bantuan ini\n"+
			"• `/stats` - Lihat statistik percakapan\n\n"+
			"*Cara menggunakan:*\n"+
			"Cukup ketik pertanyaan Anda.\n\n"+
			"📚 Anda memiliki akses ke *%d room*.\n\n"+
			"💡 *Tips:*\n"+
			"• Gunakan `/clear` sebelum tanya topik berbeda\n"+
			"• Bot ingat 10 pesan terakhir untuk konteks",
			len(links))
		return &model.RouteResult{Message: msg, RouteType: "help"}
	}

	// ---- /stats ----
	if isOneOf(msgLower, "/stats", "/statistik", "stats", "statistik", "/info") {
		total, _ := r.db.CountTotalMessages(ctx, variants)
		sessions, _ := r.db.CountDistinctSessions(ctx, variants)
		sessionID := r.getOrCreateSessionID(ctx, lookupID, variants)
		current, _ := r.db.CountSessionMessages(ctx, variants, sessionID)
		lastTime, _ := r.db.GetLastMessageTime(ctx, variants)
		lastStr := "Tidak ada"
		if lastTime != nil {
			lastStr = time.Since(*lastTime).Round(time.Minute).String() + " yang lalu"
		}
		msg := fmt.Sprintf("📊 *Statistik Percakapan Anda*\n\n"+
			"📨 Total pesan (semua session): %d\n"+
			"🔖 Session saat ini: %d pesan\n"+
			"📚 Total session: %d\n"+
			"🕐 Pesan terakhir: %s\n\n"+
			"💡 Gunakan `/clear` untuk mulai session baru",
			total, current, sessions, lastStr)
		return &model.RouteResult{Message: msg, RouteType: "command_stats"}
	}

	return nil
}

// buildPersonalizedIntro returns a personalized bot introduction for a user.
func (r *RoutingService) buildPersonalizedIntro(ctx context.Context, job model.MessageJob) string {
	lookupID := job.ChatID
	if !job.IsGroup {
		lookupID = job.PhoneNumber
	}
	variants := phoneVariants(lookupID)
	links, err := r.db.FindChatbotLinksByPhone(ctx, variants)
	if err != nil {
		r.log.Warn("buildPersonalizedIntro: FindChatbotLinks failed", zap.Error(err))
	}

	// Load resources for each room (best-effort; ignore errors)
	for _, l := range links {
		if l.Room == nil {
			continue
		}
		res, err := r.db.FindActiveResourcesByRoom(ctx, l.Room.ID)
		if err == nil {
			for _, dr := range res {
				l.Room.Resources = append(l.Room.Resources, *dr)
			}
		}
	}
	return r.ai.GetPersonalizedIntro(links)
}

// handleSendResult tracks WAHA errors and triggers auto-restart if configured.
func (r *RoutingService) handleSendResult(ctx context.Context, result model.SendResult, chatID string) {
	if result.Success {
		_ = r.cache.ResetWAHAErrors(ctx)
		return
	}
	r.log.Error("WAHA send failed", zap.String("chat_id", chatID), zap.String("error", result.Error))

	// Track consecutive errors
	if r.wahaAutoRestart {
		n, _ := r.cache.IncrWAHAErrors(ctx, 10*time.Minute)
		if n >= r.wahaMaxErrors {
			r.log.Warn("WAHA consecutive errors threshold reached, restarting session",
				zap.Int64("count", n))
			restartResult := r.waha.RestartSession(ctx)
			if restartResult.Success {
				_ = r.cache.ResetWAHAErrors(ctx)
				r.log.Info("WAHA session restarted")
			}
		}
	}
}

// appendHistory saves a Q&A exchange to the Redis conversation cache.
func (r *RoutingService) appendHistory(ctx context.Context, key, question, answer string) {
	if err := r.cache.AppendConversationHistory(ctx, key, question, answer, 20, r.historyTTL); err != nil {
		r.log.Warn("AppendConversationHistory failed", zap.Error(err))
	}
}

// getOrCreateSessionID returns the current session ID for an identifier,
// creating a new one from the database or generating a fresh ID.
func (r *RoutingService) getOrCreateSessionID(ctx context.Context, identifier string, variants []string) string {
	// Check Redis first.
	if sessionID, err := r.cache.GetSession(ctx, identifier); err == nil && sessionID != "" {
		return sessionID
	}

	// Fall back to database.
	sessionID, err := r.db.GetLastSessionID(ctx, variants)
	if err != nil || sessionID == "" {
		sessionID = newSessionID(identifier)
	}

	_ = r.cache.SetSession(ctx, identifier, sessionID, r.sessionTTL)
	return sessionID
}

// ---- Phone number normalisation --------------------------------------------

var nonDigit = regexp.MustCompile(`[^0-9]`)

// phoneVariants returns all reasonable formats of a phone number or group ID.
func phoneVariants(id string) []string {
	if strings.Contains(id, "@g.us") {
		return []string{id}
	}
	digits := nonDigit.ReplaceAllString(id, "")
	seen := map[string]struct{}{}
	add := func(s string) {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	add(digits)
	if strings.HasPrefix(digits, "62") {
		add("0" + digits[2:])
		add(digits[2:])
	} else if strings.HasPrefix(digits, "0") {
		add("62" + digits[1:])
		add(digits[1:])
	} else if strings.HasPrefix(digits, "8") {
		add("0" + digits)
		add("62" + digits)
	}
	result := make([]string, 0, len(seen))
	for k := range seen {
		result = append(result, k)
	}
	return result
}

// newSessionID generates a new random session ID.
func newSessionID(identifier string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("session_%d_%s", time.Now().Unix(), hex.EncodeToString(b))
}

// waHistoryToPairs converts a slice of WhatsApp history rows to ConvPairs.
func waHistoryToPairs(history []*model.WhatsAppConversationHistory) []model.ConvPair {
	pairs := make([]model.ConvPair, 0, len(history))
	for _, h := range history {
		pairs = append(pairs, model.ConvPair{
			User:      h.UserMessage,
			Assistant: h.BotResponse,
		})
	}
	return pairs
}

// isOneOf returns true if s equals any of the given candidates.
func isOneOf(s string, candidates ...string) bool {
	for _, c := range candidates {
		if s == c {
			return true
		}
	}
	return false
}
