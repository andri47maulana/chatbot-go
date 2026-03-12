// Package repository provides database access functions.
package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aigri/whatsapp-bot/internal/model"
	// mysql driver registered via init()
	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

// DB wraps *sql.DB and exposes all query methods used across the service.
type DB struct {
	db  *sql.DB
	log *zap.Logger
}

// New opens a MySQL connection pool using the provided DSN.
func New(dsn string, maxOpen, maxIdle int, log *zap.Logger) (*DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &DB{db: db, log: log}, nil
}

// Close closes the underlying connection pool.
func (r *DB) Close() error { return r.db.Close() }

// ---- ChatbotLinks -----------------------------------------------------------

// FindChatbotLinksByPhone returns all active chatbot links whose phone matches
// any variant in the provided slice.  Each link is returned with its Room.
func (r *DB) FindChatbotLinksByPhone(ctx context.Context, variants []string) ([]*model.ChatbotLink, error) {
	if len(variants) == 0 {
		return nil, nil
	}

	// Build IN (?,?,…) placeholder
	ph := make([]string, len(variants))
	args := make([]interface{}, len(variants))
	for i, v := range variants {
		ph[i] = "?"
		args[i] = v
	}
	inClause := ""
	for i, p := range ph {
		if i > 0 {
			inClause += ","
		}
		inClause += p
	}

	query := fmt.Sprintf(`
		SELECT cl.id, cl.phone, cl.name, cl.room_id, cl.is_active, cl.last_accessed_at,
		       r.id, r.name, r.description
		FROM chatbot_links cl
		JOIN rooms r ON r.id = cl.room_id
		WHERE cl.phone IN (%s) AND cl.is_active = 1`, inClause)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("FindChatbotLinksByPhone: %w", err)
	}
	defer rows.Close()

	var links []*model.ChatbotLink
	for rows.Next() {
		cl := &model.ChatbotLink{Room: &model.Room{}}
		var lat *time.Time
		if err := rows.Scan(
			&cl.ID, &cl.Phone, &cl.Name, &cl.RoomID, &cl.IsActive, &lat,
			&cl.Room.ID, &cl.Room.Name, &cl.Room.Description,
		); err != nil {
			return nil, fmt.Errorf("scan chatbot_link: %w", err)
		}
		cl.LastAccessedAt = lat
		links = append(links, cl)
	}
	return links, rows.Err()
}

// UpdateLastAccessed bumps the last_accessed_at timestamp.
func (r *DB) UpdateLastAccessed(ctx context.Context, linkID int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE chatbot_links SET last_accessed_at = ? WHERE id = ?`,
		time.Now(), linkID)
	return err
}

// ---- Rooms / Resources ------------------------------------------------------

// FindActiveResourcesByRoom returns all is_active data resources for a room.
func (r *DB) FindActiveResourcesByRoom(ctx context.Context, roomID int64) ([]*model.DataResource, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, room_id, title, type, is_active
		 FROM data_resources WHERE room_id = ? AND is_active = 1`, roomID)
	if err != nil {
		return nil, fmt.Errorf("FindActiveResources: %w", err)
	}
	defer rows.Close()

	var res []*model.DataResource
	for rows.Next() {
		dr := &model.DataResource{}
		if err := rows.Scan(&dr.ID, &dr.RoomID, &dr.Title, &dr.Type, &dr.IsActive); err != nil {
			return nil, fmt.Errorf("scan resource: %w", err)
		}
		res = append(res, dr)
	}
	return res, rows.Err()
}

// ---- Conversation History (room-level) -------------------------------------

// InsertConversation creates a new conversation record and returns its ID.
func (r *DB) InsertConversation(ctx context.Context, c *model.ConversationHistory) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO conversation_histories (room_id, user_id, thread_id, query, llm_response, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.RoomID, c.UserID, c.ThreadID, c.Query, c.LLMResponse, time.Now())
	if err != nil {
		return 0, fmt.Errorf("InsertConversation: %w", err)
	}
	return res.LastInsertId()
}

// UpdateConversationResponse writes the AI response back to a conversation row.
func (r *DB) UpdateConversationResponse(ctx context.Context, id int64, response string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE conversation_histories SET llm_response = ? WHERE id = ?`, response, id)
	return err
}

// GetConversationContext returns the last N Q&A pairs for a thread.
func (r *DB) GetConversationContext(ctx context.Context, roomID, userID int64, threadID string, limit int) ([]model.ConvPair, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT query, llm_response
		 FROM conversation_histories
		 WHERE room_id = ? AND user_id = ? AND thread_id = ?
		 ORDER BY created_at DESC LIMIT ?`,
		roomID, userID, threadID, limit)
	if err != nil {
		return nil, fmt.Errorf("GetConversationContext: %w", err)
	}
	defer rows.Close()

	var pairs []model.ConvPair
	for rows.Next() {
		var p model.ConvPair
		if err := rows.Scan(&p.User, &p.Assistant); err != nil {
			return nil, fmt.Errorf("scan conv pair: %w", err)
		}
		pairs = append(pairs, p)
	}
	// Reverse to chronological order
	for i, j := 0, len(pairs)-1; i < j; i, j = i+1, j-1 {
		pairs[i], pairs[j] = pairs[j], pairs[i]
	}
	return pairs, rows.Err()
}

// ---- WhatsApp conversation history -----------------------------------------

// GetLastSessionID returns the most recent session_id for the given phone variants.
func (r *DB) GetLastSessionID(ctx context.Context, variants []string) (string, error) {
	if len(variants) == 0 {
		return "", nil
	}
	args := make([]interface{}, len(variants))
	ph := make([]string, len(variants))
	for i, v := range variants {
		ph[i] = "?"
		args[i] = v
	}
	inClause := ""
	for i, p := range ph {
		if i > 0 {
			inClause += ","
		}
		inClause += p
	}
	var sessionID sql.NullString
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT session_id FROM whatsapp_conversation_histories
		             WHERE phone_number IN (%s)
		             ORDER BY created_at DESC LIMIT 1`, inClause),
		args...).Scan(&sessionID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetLastSessionID: %w", err)
	}
	return sessionID.String, nil
}

// InsertWhatsAppHistory saves a WhatsApp Q&A exchange.
func (r *DB) InsertWhatsAppHistory(ctx context.Context, h *model.WhatsAppConversationHistory) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO whatsapp_conversation_histories
		 (phone_number, session_id, user_message, bot_response, route_type, kategori, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		h.PhoneNumber, h.SessionID, h.UserMessage, h.BotResponse,
		h.RouteType, h.Kategori, time.Now())
	if err != nil {
		return fmt.Errorf("InsertWhatsAppHistory: %w", err)
	}
	return nil
}

// GetWhatsAppHistoryBySession returns recent messages for a session in chronological order.
func (r *DB) GetWhatsAppHistoryBySession(ctx context.Context, variants []string, sessionID string, limit int) ([]*model.WhatsAppConversationHistory, error) {
	if len(variants) == 0 {
		return nil, nil
	}
	args := make([]interface{}, 0, len(variants)+2)
	ph := make([]string, len(variants))
	for i, v := range variants {
		ph[i] = "?"
		args = append(args, v)
	}
	args = append(args, sessionID, limit)
	inClause := ""
	for i, p := range ph {
		if i > 0 {
			inClause += ","
		}
		inClause += p
	}

	rows, err := r.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT phone_number, session_id, user_message, bot_response, route_type, kategori, created_at
		             FROM whatsapp_conversation_histories
		             WHERE phone_number IN (%s) AND session_id = ?
		             ORDER BY created_at DESC LIMIT ?`, inClause),
		args...)
	if err != nil {
		return nil, fmt.Errorf("GetWhatsAppHistoryBySession: %w", err)
	}
	defer rows.Close()

	var list []*model.WhatsAppConversationHistory
	for rows.Next() {
		h := &model.WhatsAppConversationHistory{}
		if err := rows.Scan(&h.PhoneNumber, &h.SessionID, &h.UserMessage, &h.BotResponse,
			&h.RouteType, &h.Kategori, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan wa history: %w", err)
		}
		list = append(list, h)
	}
	// Reverse to chronological
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, rows.Err()
}

// CountSessionMessages returns how many messages exist in a session.
func (r *DB) CountSessionMessages(ctx context.Context, variants []string, sessionID string) (int, error) {
	if len(variants) == 0 {
		return 0, nil
	}
	args := make([]interface{}, 0, len(variants)+1)
	ph := make([]string, len(variants))
	for i, v := range variants {
		ph[i] = "?"
		args = append(args, v)
	}
	args = append(args, sessionID)
	inClause := ""
	for i, p := range ph {
		if i > 0 {
			inClause += ","
		}
		inClause += p
	}
	var count int
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM whatsapp_conversation_histories
		             WHERE phone_number IN (%s) AND session_id = ?`, inClause),
		args...).Scan(&count)
	return count, err
}

// CountDistinctSessions returns total number of distinct sessions for a set of phone variants.
func (r *DB) CountDistinctSessions(ctx context.Context, variants []string) (int, error) {
	if len(variants) == 0 {
		return 0, nil
	}
	args := make([]interface{}, len(variants))
	ph := make([]string, len(variants))
	for i, v := range variants {
		ph[i] = "?"
		args[i] = v
	}
	inClause := ""
	for i, p := range ph {
		if i > 0 {
			inClause += ","
		}
		inClause += p
	}
	var count int
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(DISTINCT session_id) FROM whatsapp_conversation_histories
		             WHERE phone_number IN (%s)`, inClause),
		args...).Scan(&count)
	return count, err
}

// CountTotalMessages returns total message count for a phone identifier set.
func (r *DB) CountTotalMessages(ctx context.Context, variants []string) (int, error) {
	if len(variants) == 0 {
		return 0, nil
	}
	args := make([]interface{}, len(variants))
	ph := make([]string, len(variants))
	for i, v := range variants {
		ph[i] = "?"
		args[i] = v
	}
	inClause := ""
	for i, p := range ph {
		if i > 0 {
			inClause += ","
		}
		inClause += p
	}
	var count int
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM whatsapp_conversation_histories WHERE phone_number IN (%s)`, inClause),
		args...).Scan(&count)
	return count, err
}

// CountRoomConversationsByThread returns the count inside room conversation_histories for a thread.
func (r *DB) CountRoomConversationsByThread(ctx context.Context, threadID string) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversation_histories WHERE thread_id = ?`, threadID).Scan(&count)
	return count, err
}

// GetLastMessageTime returns the last created_at for a phone variant set.
func (r *DB) GetLastMessageTime(ctx context.Context, variants []string) (*time.Time, error) {
	if len(variants) == 0 {
		return nil, nil
	}
	args := make([]interface{}, len(variants))
	ph := make([]string, len(variants))
	for i, v := range variants {
		ph[i] = "?"
		args[i] = v
	}
	inClause := ""
	for i, p := range ph {
		if i > 0 {
			inClause += ","
		}
		inClause += p
	}
	var t time.Time
	err := r.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT created_at FROM whatsapp_conversation_histories WHERE phone_number IN (%s) ORDER BY created_at DESC LIMIT 1`, inClause),
		args...).Scan(&t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ---- Virtual users ---------------------------------------------------------

// GetOrCreateVirtualUser finds or inserts a virtual user for a chatbot link phone.
func (r *DB) GetOrCreateVirtualUser(ctx context.Context, phone, displayName string) (*model.User, error) {
	email := fmt.Sprintf("chatbot_%s@virtual.local", phone)

	var u model.User
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, email FROM users WHERE email = ? LIMIT 1`, email).
		Scan(&u.ID, &u.Name, &u.Email)
	if err == nil {
		// Ensure name is up-to-date
		if u.Name != displayName+" (WhatsApp)" {
			_, _ = r.db.ExecContext(ctx,
				`UPDATE users SET name = ? WHERE id = ?`, displayName+" (WhatsApp)", u.ID)
			u.Name = displayName + " (WhatsApp)"
		}
		return &u, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("GetOrCreateVirtualUser: %w", err)
	}

	// Create
	passHash := randomHex(16) // not a real login, just a placeholder
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO users (name, email, password, email_verified_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		displayName+" (WhatsApp)", email, passHash, time.Now(), time.Now(), time.Now())
	if err != nil {
		return nil, fmt.Errorf("insert virtual user: %w", err)
	}
	id, _ := res.LastInsertId()
	return &model.User{ID: id, Name: displayName + " (WhatsApp)", Email: email}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
