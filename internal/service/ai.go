// Package service contains OpenAI-powered helpers used by the routing service.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aigri/whatsapp-bot/internal/model"
	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"
)

// AIHelper provides thin wrappers around OpenAI chat completions.
type AIHelper struct {
	client *openai.Client
	model  string
	log    *zap.Logger
}

// NewAIHelper creates a new AIHelper.
func NewAIHelper(apiKey, model string, log *zap.Logger) *AIHelper {
	return &AIHelper{
		client: openai.NewClient(apiKey),
		model:  model,
		log:    log,
	}
}

// IsIdentityQuestion returns true if the message is asking about the bot's identity.
func (a *AIHelper) IsIdentityQuestion(ctx context.Context, message string) bool {
	msgLower := strings.ToLower(strings.TrimSpace(message))

	// Tier 1: fast keyword check (no network required).
	patterns := []string{
		"kamu siapa", "siapa kamu", "siapa anda", "anda siapa",
		"nama kamu", "kamu nama", "nama anda", "namamu", "nama mu",
		"perkenalkan diri", "kenalan dong", "kenalan dulu", "mau kenal",
		"bot apa", "ai apa", "chatbot apa", "asisten apa",
		"bisa apa", "fungsi apa", "tugas apa", "untuk apa",
		"who are you", "what are you", "your name", "what can you do",
		"introduce yourself", "tell me about yourself",
	}
	for _, p := range patterns {
		if strings.Contains(msgLower, p) {
			return true
		}
	}

	// Tier 2: AI semantic check for short messages.
	if len(msgLower) <= 150 && a.containsIdentityKeyword(msgLower) {
		result, err := a.chatComplete(ctx, openai.ChatCompletionRequest{
			Model: a.model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role: openai.ChatMessageRoleSystem,
					Content: "You are a message classifier. Determine if the user is asking about the bot's identity, name, purpose, or capabilities. Reply ONLY with 'yes' or 'no'.",
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: fmt.Sprintf("Message: %q\n\nIs this asking about bot identity/purpose/capabilities?", message),
				},
			},
			MaxTokens:   10,
			Temperature: 0.1,
		})
		if err == nil && strings.ToLower(strings.TrimSpace(result)) == "yes" {
			return true
		}
	}
	return false
}

// IsSimpleGreeting returns true for very short or greeting-only messages.
func (a *AIHelper) IsSimpleGreeting(message string) bool {
	msg := strings.ToLower(strings.TrimSpace(message))
	if len(msg) < 10 {
		return true
	}
	greetings := []string{
		"halo", "hai", "hi ", "hello", "hey ",
		"terima kasih", "thanks", "makasih",
		"apa kabar", "selamat pagi", "selamat siang", "selamat sore", "selamat malam",
		"good morning", "good afternoon", "good evening",
		"ok", "oke", "ya", "baik", "siap",
		"assalamualaikum",
	}
	for _, g := range greetings {
		if msg == g || (strings.HasPrefix(msg, g) && len(msg) < 25 && !containsQuestionWords(msg)) {
			return true
		}
	}
	return false
}

// QuickLLMResponse generates a short conversational reply for greetings.
func (a *AIHelper) QuickLLMResponse(ctx context.Context, message string) (string, error) {
	return a.chatComplete(ctx, openai.ChatCompletionRequest{
		Model: a.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleSystem,
				Content: "Anda adalah AIGRI Assistant, asisten AI untuk PTPN 1 (perusahaan perkebunan Indonesia).\n" +
					"Balas dengan ramah dan singkat dalam Bahasa Indonesia. Maksimal 2 kalimat.",
			},
			{Role: openai.ChatMessageRoleUser, Content: message},
		},
		MaxTokens:   100,
		Temperature: 0.7,
	})
}

// SelectBestRoom uses LLM to pick the most relevant room for a query.
// Returns index into links, or 0 on any error (safe fallback).
func (a *AIHelper) SelectBestRoom(ctx context.Context, query string, links []*model.ChatbotLink) int {
	if len(links) <= 1 {
		return 0
	}

	type roomItem struct {
		Index       int    `json:"index"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	rooms := make([]roomItem, 0, len(links))
	for i, l := range links {
		if l.Room == nil {
			continue
		}
		desc := l.Room.Description
		if desc == "" {
			desc = "Knowledge room tanpa deskripsi"
		}
		rooms = append(rooms, roomItem{Index: i, Name: l.Room.Name, Description: desc})
	}

	roomsJSON, _ := json.Marshal(rooms)
	systemPrompt := fmt.Sprintf(
		"Anda adalah AI yang membantu memilih Knowledge Room terbaik untuk menjawab pertanyaan user.\n\n"+
			"Rooms yang tersedia:\n%s\n\n"+
			"Berikan response dalam format JSON:\n"+
			`{"selected_index": 0, "room_name": "NAMA ROOM", "confidence": "high/medium/low", "reason": "alasan"}`,
		string(roomsJSON))

	content, err := a.chatComplete(ctx, openai.ChatCompletionRequest{
		Model: a.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: "Pertanyaan: " + query + "\n\nPilih room yang paling tepat.",
			},
		},
		MaxTokens:   200,
		Temperature: 0.3,
	})
	if err != nil {
		a.log.Warn("LLM room selection failed, fallback index=0", zap.Error(err))
		return 0
	}

	// Strip markdown code fences if present.
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		if len(lines) > 2 {
			content = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var sel struct {
		SelectedIndex int    `json:"selected_index"`
		RoomName      string `json:"room_name"`
		Confidence    string `json:"confidence"`
		Reason        string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(content), &sel); err != nil {
		a.log.Warn("parse room selection response failed", zap.Error(err))
		return 0
	}
	if sel.SelectedIndex < 0 || sel.SelectedIndex >= len(links) {
		a.log.Warn("invalid room index", zap.Int("index", sel.SelectedIndex))
		return 0
	}
	a.log.Info("LLM room selected",
		zap.String("room", sel.RoomName),
		zap.Int("index", sel.SelectedIndex),
		zap.String("confidence", sel.Confidence),
	)
	return sel.SelectedIndex
}

// TransformQuery rewrites a short/ambiguous follow-up query using conversation context.
// Returns the original query unchanged on any error.
func (a *AIHelper) TransformQuery(ctx context.Context, query string, history []model.ConvPair) string {
	if len(history) == 0 || len(query) >= 300 {
		return query
	}

	msgs := []openai.ChatCompletionMessage{
		{
			Role: openai.ChatMessageRoleSystem,
			Content: "Anda adalah AI yang membantu memperjelas pertanyaan user berdasarkan konteks percakapan sebelumnya.\n\n" +
				"TUGAS: Jika pertanyaan user terlalu singkat atau ambigu, perjelas dengan menambahkan konteks dari percakapan sebelumnya.\n\n" +
				"ATURAN:\n" +
				"1. Jika pertanyaan sudah jelas, kembalikan pertanyaan asli\n" +
				"2. Jika singkat/ambigu, perjelas dengan konteks sebelumnya\n" +
				"3. Gunakan bahasa Indonesia\n" +
				"4. Jangan tambahkan informasi yang tidak ada di konteks\n" +
				"5. Hanya kembalikan pertanyaan yang diperjelas, tanpa penjelasan tambahan",
		},
	}
	for _, p := range history {
		if p.User != "" {
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: p.User,
			})
		}
		if p.Assistant != "" {
			content := p.Assistant
			if len(content) > 200 {
				content = content[:200]
			}
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: content,
			})
		}
	}
	msgs = append(msgs, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: query,
	})

	transformed, err := a.chatComplete(ctx, openai.ChatCompletionRequest{
		Model:       a.model,
		Messages:    msgs,
		MaxTokens:   150,
		Temperature: 0.3,
	})
	if err != nil {
		a.log.Warn("TransformQuery failed", zap.Error(err))
		return query
	}
	transformed = strings.TrimSpace(strings.Trim(transformed, `"'`))
	if transformed == "" || len(transformed) > len(query)*10 {
		return query
	}
	return transformed
}

// DetectTopicChange returns true if the new query represents a significant topic shift.
func (a *AIHelper) DetectTopicChange(ctx context.Context, newQuery string, history []model.ConvPair) bool {
	if len(history) == 0 {
		return false
	}
	prev := make([]string, 0, 3)
	for _, p := range history {
		if p.User != "" {
			prev = append(prev, p.User)
		}
	}
	if len(prev) > 3 {
		prev = prev[len(prev)-3:]
	}

	answer, err := a.chatComplete(ctx, openai.ChatCompletionRequest{
		Model: a.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleSystem,
				Content: "Anda adalah asisten yang mendeteksi perubahan topik dalam percakapan.\n" +
					"Return 'YES' jika topik berubah SIGNIFIKAN.\n" +
					"Return 'NO' jika masih topik sama, terkait, atau merupakan pertanyaan lanjutan.\n" +
					"HANYA return 'YES' atau 'NO'.",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("Sebelumnya:\n%s\n\nBaru:\n%s\n\nTopik berubah signifikan?", strings.Join(prev, " | "), newQuery),
			},
		},
		MaxTokens:   10,
		Temperature: 0.3,
	})
	if err != nil {
		return false
	}
	return strings.ToUpper(strings.TrimSpace(answer)) == "YES"
}

// ValidateQueryClarity reports whether a query is clear enough to answer,
// and if not, returns a user-facing clarification request.
func (a *AIHelper) ValidateQueryClarity(query string) (bool, string) {
	queryLower := strings.ToLower(query)
	var issues []string

	if strings.Count(query, "?") > 1 {
		issues = append(issues, "multiple_questions")
	}
	for _, p := range []string{" atau ", " dan ", " serta "} {
		if strings.Contains(queryLower, p) {
			issues = append(issues, "compound_question")
			break
		}
	}
	for _, p := range []string{"yang mana", "seperti apa", "itu apa", "maksudnya", "yang bagaimana"} {
		if strings.Contains(queryLower, p) {
			issues = append(issues, "vague_reference")
			break
		}
	}

	if len(issues) == 0 {
		return true, ""
	}
	msg := fmt.Sprintf("❓ Pertanyaan Anda masih belum jelas:\n\n_%s_\n\nSilakan ulangi pertanyaan dengan lebih spesifik.", query)
	return false, msg
}

// GetPersonalizedIntro builds a welcome message listing the user's accessible rooms.
func (a *AIHelper) GetPersonalizedIntro(links []*model.ChatbotLink) string {
	if len(links) == 0 {
		return "🤖 *AIGRI Assistant - AI untuk PTPN 1*\n\n" +
			"Saya adalah asisten AI yang siap membantu Anda.\n\n" +
			"⚠️ Anda belum memiliki akses ke Knowledge Room.\n" +
			"Silakan hubungi administrator untuk mendapatkan akses.\n\nTerima kasih! 😊"
	}

	emojis := []string{"🎯", "📊", "👥", "📄", "🌱", "🏭", "💼", "🔧", "📈", "🌾"}
	var sb strings.Builder
	sb.WriteString("🤖 *AIGRI Assistant - AI untuk PTPN 1*\n\nSaya adalah asisten AI personal Anda yang dapat membantu dengan:\n\n")

	for i, link := range links {
		if link.Room == nil {
			continue
		}
		emoji := emojis[i%len(emojis)]
		sb.WriteString(fmt.Sprintf("%s *%s*\n", emoji, link.Room.Name))
		if link.Room.Description != "" {
			sb.WriteString(fmt.Sprintf("   _%s_\n", link.Room.Description))
		}
		if len(link.Room.Resources) > 0 {
			sb.WriteString(fmt.Sprintf("   📚 %d sumber data tersedia\n", len(link.Room.Resources)))
			titles := make([]string, 0, 3)
			for _, r := range link.Room.Resources {
				if len(titles) < 3 {
					titles = append(titles, r.Title)
				}
			}
			sb.WriteString("   • " + strings.Join(titles, "\n   • ") + "\n")
			if len(link.Room.Resources) > 3 {
				sb.WriteString(fmt.Sprintf("   • _dan %d lainnya..._\n", len(link.Room.Resources)-3))
			}
		}
		sb.WriteString("\n")
	}
	sb.WriteString("💡 *Cara Menggunakan:*\nCukup ajukan pertanyaan Anda, dan saya akan otomatis memilih Knowledge Room yang paling tepat!\n\n")
	sb.WriteString("Contoh:\n• _\"Siapa Kepala Kebun Jember?\"_\n• _\"Berapa produksi kopi bulan ini?\"_\n\nSilakan tanyakan apa saja! 😊")
	return sb.String()
}

// ---- private helpers --------------------------------------------------------

func (a *AIHelper) chatComplete(ctx context.Context, req openai.ChatCompletionRequest) (string, error) {
	resp, err := a.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", fmt.Errorf("openai: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}

func (a *AIHelper) containsIdentityKeyword(msg string) bool {
	for _, k := range []string{
		"kamu", "anda", "lu", "loe", "situ", "ente",
		"you", "your", "ur",
		"siapa", "who", "what", "apa",
		"nama", "name",
		"bot", "ai", "asisten", "assistant", "chatbot",
		"kenalan", "kenal", "introduce",
		"bisa", "can", "bantu", "help",
		"fungsi", "function", "tugas", "purpose",
	} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

func containsQuestionWords(msg string) bool {
	for _, w := range []string{
		"siapa", "apa", "dimana", "kapan", "kenapa", "bagaimana", "berapa", "data", "info",
	} {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}
