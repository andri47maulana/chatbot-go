// Package model contains all shared domain types used across the application.
package model

import "time"

// ---- Database models --------------------------------------------------------

// ChatbotLink represents a shared access record: a phone/group has access to a Room.
type ChatbotLink struct {
	ID             int64      `db:"id"`
	Phone          string     `db:"phone"`
	Name           string     `db:"name"`
	RoomID         int64      `db:"room_id"`
	IsActive       bool       `db:"is_active"`
	LastAccessedAt *time.Time `db:"last_accessed_at"`
	CreatedAt      time.Time  `db:"created_at"`
	// Joined
	Room *Room
}

// Room represents a Knowledge Room.
type Room struct {
	ID          int64     `db:"id"`
	Name        string    `db:"name"`
	Description string    `db:"description"`
	CreatedAt   time.Time `db:"created_at"`
	// Joined
	Resources []DataResource
}

// DataResource is a file/resource inside a room.
type DataResource struct {
	ID       int64  `db:"id"`
	RoomID   int64  `db:"room_id"`
	Title    string `db:"title"`
	Type     string `db:"type"`
	IsActive bool   `db:"is_active"`
}

// ConversationHistory is the room-level conversation log.
type ConversationHistory struct {
	ID          int64     `db:"id"`
	RoomID      int64     `db:"room_id"`
	UserID      int64     `db:"user_id"`
	ThreadID    string    `db:"thread_id"`
	Query       string    `db:"query"`
	LLMResponse string    `db:"llm_response"`
	CreatedAt   time.Time `db:"created_at"`
}

// WhatsAppConversationHistory is per-session WhatsApp history.
type WhatsAppConversationHistory struct {
	ID             int64     `db:"id"`
	PhoneNumber    string    `db:"phone_number"`
	SessionID      string    `db:"session_id"`
	UserMessage    string    `db:"user_message"`
	BotResponse    string    `db:"bot_response"`
	RouteType      string    `db:"route_type"`
	Kategori       string    `db:"kategori"`
	CreatedAt      time.Time `db:"created_at"`
}

// User is a virtual or real user record.
type User struct {
	ID    int64  `db:"id"`
	Name  string `db:"name"`
	Email string `db:"email"`
}

// ---- Webhook payload types --------------------------------------------------

// WAHAEvent is the outer envelope WAHA sends to the webhook.
type WAHAEvent struct {
	Event   string       `json:"event"`
	Payload *WAHAMessage `json:"payload"`
	Me      *WAHAMe      `json:"me"`
	Session string       `json:"session"`
	Engine  string       `json:"engine"` // "NOWEB" or "WEBJS"
}

// WAHAMe describes the bot's own WhatsApp identity.
type WAHAMe struct {
	ID       string `json:"id"`
	LID      string `json:"lid"`
	PushName string `json:"pushName"`
}

// WAHAMessage is the inner message data.
type WAHAMessage struct {
	ID           string            `json:"id"`
	From         string            `json:"from"`
	To           string            `json:"to"`
	Body         string            `json:"body"`
	Caption      string            `json:"caption"`
	FromMe       bool              `json:"fromMe"`
	Author       string            `json:"author"`
	Participant  string            `json:"participant"`
	ChatID       string            `json:"chatId"`
	Sender       string            `json:"sender"`
	NotifyName   string            `json:"notifyName"`
	PushName     string            `json:"pushName"`
	MentionedIDs []string          `json:"mentionedIds"`
	QuotedMsg    *WAHAQuotedMsg    `json:"quotedMsg"`
	ExtendedText *WAHAExtTextData  `json:"extendedText"` // WAHA flat extended-text
	InternalData *WAHAInternalData `json:"_data"`
	Me           *WAHAMe           `json:"me"` // WAHA NOWEB: bot identity in message
	// event type when WAHA sends flat (no nested payload)
	Event string `json:"event"`
}

// WAHAExtTextData is the flat extended-text field.
type WAHAExtTextData struct {
	Text string `json:"text"`
}

// WAHAQuotedMsg is a reply-to reference.
type WAHAQuotedMsg struct {
	FromMe bool `json:"fromMe"`
}

// WAHAInternalData is the _data field from WAHA.
type WAHAInternalData struct {
	ID              string            `json:"id"`
	RemoteJid       string            `json:"remoteJid"`       // WEBJS
	RemoteJidAlt    string            `json:"remoteJidAlt"`    // NOWEB
	Key             *WAHAKey          `json:"key"`
	Message         *WAHADataMessage  `json:"message"`
	MentionedJidList []string         `json:"mentionedJidList"`
}

// WAHAKey holds remoteJid fields used for NOWEB phone extraction.
type WAHAKey struct {
	RemoteJid    string `json:"remoteJid"`
	RemoteJidAlt string `json:"remoteJidAlt"`
}

// WAHADataMessage is nested message data for NOWEB mention extraction.
type WAHADataMessage struct {
	ExtendedTextMessage *WAHAExtendedText `json:"extendedTextMessage"`
}

// WAHAExtendedText holds context info with mentioned JIDs.
type WAHAExtendedText struct {
	ContextInfo *WAHAContextInfo `json:"contextInfo"`
}

// WAHAContextInfo holds the list of mentioned JIDs.
type WAHAContextInfo struct {
	MentionedJid []string `json:"mentionedJid"`
}

// ---- Service result types ---------------------------------------------------

// RouteResult is returned by the routing service after processing a message.
type RouteResult struct {
	Success      bool
	Message      string
	Source       string
	RouteType    string
	RoomID       int64
	RoomName     string
	SessionID    string
}

// SendResult is returned by WahaService.SendText.
type SendResult struct {
	Success   bool
	MessageID string
	Error     string
	Retries   int
}

// ResourceExecutionResult is what the RAG/resource executor returns.
type ResourceExecutionResult struct {
	Success  bool
	Answer   string
	Sources  []ResourceSource
	Error    string
}

// ResourceSource gives provenance info from RAG.
type ResourceSource struct {
	Source string `json:"source"`
	Title  string `json:"title"`
}

// ConvPair represents one Q&A exchange in conversation context.
type ConvPair struct {
	User      string
	Assistant string
}

// MessageJob is the unit of work handed off to the worker pool.
type MessageJob struct {
	PhoneNumber string
	ChatID      string
	IsGroup     bool
	MessageBody string
	MessageID   string
	DisplayName string
	ReceivedAt  time.Time
}
