// Package client contains HTTP clients for external services.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aigri/whatsapp-bot/internal/model"
	"go.uber.org/zap"
)

// ---- WAHA client ------------------------------------------------------------

// WAHAClient talks to the WAHA WhatsApp HTTP API.
type WAHAClient struct {
	baseURL string
	apiKey  string
	session string
	hc      *http.Client
	log     *zap.Logger
}

// NewWAHAClient creates a WAHAClient with sensible timeouts and connection reuse.
func NewWAHAClient(baseURL, apiKey, session string, log *zap.Logger) *WAHAClient {
	return &WAHAClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		session: session,
		log:     log,
		hc: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				DisableKeepAlives:   false,
			},
		},
	}
}

type sendTextReq struct {
	ChatID string `json:"chatId"`
	Text   string `json:"text"`
}

// SendText sends a plain text message to chatId (supports both personal and group IDs).
func (w *WAHAClient) SendText(ctx context.Context, chatID, text string) model.SendResult {
	if chatID == "" || text == "" {
		return model.SendResult{Error: "chatID and text are required"}
	}

	payload := sendTextReq{ChatID: chatID, Text: text}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("%s/api/sendText", w.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return model.SendResult{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if w.apiKey != "" {
		req.Header.Set("X-Api-Key", w.apiKey)
	}

	resp, err := w.hc.Do(req)
	if err != nil {
		return model.SendResult{Error: err.Error()}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return model.SendResult{
			Error: fmt.Sprintf("WAHA %d: %s", resp.StatusCode, string(respBody)),
		}
	}

	var result struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(respBody, &result)

	w.log.Info("WAHA message sent",
		zap.String("chat_id", chatID),
		zap.String("message_id", result.ID),
	)
	return model.SendResult{Success: true, MessageID: result.ID}
}

type restartReq struct {
	Session string `json:"session"`
}

// RestartSession restarts the WAHA session.
func (w *WAHAClient) RestartSession(ctx context.Context) model.SendResult {
	payload := restartReq{Session: w.session}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("%s/api/sessions/%s/restart", w.baseURL, w.session)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return model.SendResult{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if w.apiKey != "" {
		req.Header.Set("X-Api-Key", w.apiKey)
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return model.SendResult{Error: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return model.SendResult{Error: fmt.Sprintf("restart %d: %s", resp.StatusCode, b)}
	}
	return model.SendResult{Success: true}
}

// ---- RAG client -------------------------------------------------------------

// RAGClient talks to the RAG backend (Qdrant-based or similar).
type RAGClient struct {
	baseURL string
	apiKey  string
	hc      *http.Client
	log     *zap.Logger
}

// NewRAGClient creates a RAGClient.
func NewRAGClient(baseURL, apiKey string, log *zap.Logger) *RAGClient {
	return &RAGClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		log:     log,
		hc: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

type ragQueryReq struct {
	Query      string            `json:"query"`
	Kategori   string            `json:"kategori,omitempty"`
	TopK       int               `json:"top_k,omitempty"`
	Context    []model.ConvPair  `json:"context,omitempty"`
	UserName   string            `json:"user_name,omitempty"`
	RoomName   string            `json:"room_name,omitempty"`
}

type ragQueryResp struct {
	Success bool                    `json:"success"`
	Answer  string                  `json:"answer"`
	Sources []model.ResourceSource  `json:"sources"`
	Error   string                  `json:"error"`
}

// Query sends a question to the RAG backend and returns the answer.
func (r *RAGClient) Query(ctx context.Context, req ragQueryReq) model.ResourceExecutionResult {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/query", bytes.NewReader(body))
	if err != nil {
		return model.ResourceExecutionResult{Error: err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		httpReq.Header.Set("X-Api-Key", r.apiKey)
	}

	resp, err := r.hc.Do(httpReq)
	if err != nil {
		return model.ResourceExecutionResult{Error: err.Error()}
	}
	defer resp.Body.Close()

	var result ragQueryResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return model.ResourceExecutionResult{Error: "decode rag response: " + err.Error()}
	}
	if !result.Success {
		return model.ResourceExecutionResult{Error: result.Error}
	}
	return model.ResourceExecutionResult{
		Success: true,
		Answer:  result.Answer,
		Sources: result.Sources,
	}
}

// SelectBestResource calls the RAG backend to select the most relevant resource.
// Falls back to returning the first resource if RAG selection fails.
func (r *RAGClient) SelectBestResource(ctx context.Context, query string, resources []*model.DataResource) *model.DataResource {
	if len(resources) == 0 {
		return nil
	}
	if len(resources) == 1 {
		return resources[0]
	}
	// Simple fallback: return first active resource
	// A real implementation would call an LLM or embedding similarity endpoint
	return resources[0]
}

// ExecuteResource sends a query to the RAG using the given resource/category context.
func (r *RAGClient) ExecuteResource(
	ctx context.Context,
	resource *model.DataResource,
	query string,
	userName, userPhone, roomName string,
	history []model.ConvPair,
) model.ResourceExecutionResult {
	return r.Query(ctx, ragQueryReq{
		Query:    query,
		Kategori: resource.Type,
		TopK:     5,
		Context:  history,
		UserName: userName,
		RoomName: roomName,
	})
}
