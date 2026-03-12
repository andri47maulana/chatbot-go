// Package cache wraps Redis operations used by the bot service.
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Client wraps a redis.Client with convenience helpers.
type Client struct {
	rdb *redis.Client
	log *zap.Logger
}

// New creates a new Cache Client.
func New(addr, password string, db int, log *zap.Logger) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     20,
		MinIdleConns: 5,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Client{rdb: rdb, log: log}, nil
}

// Close closes the Redis connection.
func (c *Client) Close() error { return c.rdb.Close() }

// ---- Message deduplication --------------------------------------------------

// MarkProcessed marks a message ID as processed with a TTL.
// Returns true if the message was successfully marked (first time).
// Returns false if it was already processed (duplicate).
// Uses SET NX for atomic compare-and-set.
func (c *Client) MarkProcessed(ctx context.Context, messageID string, ttl time.Duration) (bool, error) {
	key := "wa_msg_processed:" + messageID
	set, err := c.rdb.SetNX(ctx, key, time.Now().Unix(), ttl).Result()
	if err != nil {
		return false, fmt.Errorf("MarkProcessed: %w", err)
	}
	return set, nil
}

// ---- Session management -----------------------------------------------------

const sessionKeyPrefix = "wa_session:"

// GetSession returns the current session ID for an identifier, or "" if none.
func (c *Client) GetSession(ctx context.Context, identifier string) (string, error) {
	val, err := c.rdb.Get(ctx, sessionKeyPrefix+identifier).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetSession: %w", err)
	}
	return val, nil
}

// SetSession stores (or refreshes) the session ID for an identifier.
func (c *Client) SetSession(ctx context.Context, identifier, sessionID string, ttl time.Duration) error {
	return c.rdb.Set(ctx, sessionKeyPrefix+identifier, sessionID, ttl).Err()
}

// ---- Bot linked device ID ---------------------------------------------------

const botDeviceKey = "bot_linked_device_id"

// GetBotLinkedDeviceID returns the cached bot linked device ID.
func (c *Client) GetBotLinkedDeviceID(ctx context.Context) (string, error) {
	val, err := c.rdb.Get(ctx, botDeviceKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return val, err
}

// SetBotLinkedDeviceID stores the bot's linked device ID for 30 days.
func (c *Client) SetBotLinkedDeviceID(ctx context.Context, deviceID string) error {
	return c.rdb.Set(ctx, botDeviceKey, deviceID, 30*24*time.Hour).Err()
}

// ---- WAHA error tracking ----------------------------------------------------

const wahaErrKey = "waha_consecutive_errors"

// IncrWAHAErrors increments the consecutive error counter and returns the new count.
func (c *Client) IncrWAHAErrors(ctx context.Context, ttl time.Duration) (int64, error) {
	pipe := c.rdb.TxPipeline()
	incrCmd := pipe.Incr(ctx, wahaErrKey)
	pipe.Expire(ctx, wahaErrKey, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incrCmd.Val(), nil
}

// ResetWAHAErrors resets the consecutive error counter to 0.
func (c *Client) ResetWAHAErrors(ctx context.Context) error {
	return c.rdb.Del(ctx, wahaErrKey).Err()
}

// GetWAHAErrors returns the current consecutive error count.
func (c *Client) GetWAHAErrors(ctx context.Context) (int64, error) {
	val, err := c.rdb.Get(ctx, wahaErrKey).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return val, err
}

// ---- Conversation history (short-term, cache-backed) -----------------------

const histKeyPrefix = "wa_history:"

// GetConversationHistory returns cached Q&A messages for a conversation key.
// Returns nil slice (not error) if nothing is cached.
func (c *Client) GetConversationHistory(ctx context.Context, conversationKey string) ([]map[string]string, error) {
	key := histKeyPrefix + conversationKey
	// Stored as JSON array using a simple list serialisation
	vals, err := c.rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, nil
	}
	result := make([]map[string]string, 0, len(vals)/2)
	for i := 0; i+1 < len(vals); i += 2 {
		result = append(result, map[string]string{
			"role":    vals[i],
			"content": vals[i+1],
		})
	}
	return result, nil
}

// AppendConversationHistory appends a Q&A exchange to the conversation history
// and trims to the last maxMessages entries (each message = role+content pair).
func (c *Client) AppendConversationHistory(ctx context.Context, conversationKey, question, answer string, maxMessages int, ttl time.Duration) error {
	key := histKeyPrefix + conversationKey
	pipe := c.rdb.TxPipeline()
	pipe.RPush(ctx, key, "user", question, "assistant", answer)
	pipe.LTrim(ctx, key, int64(-(maxMessages*2)), -1)
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// ---- Generic get/set --------------------------------------------------------

// Get returns a string value from Redis or "" if not found.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	val, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return val, err
}

// Set stores a string value in Redis.
func (c *Client) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// Del removes a key.
func (c *Client) Del(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, key).Err()
}
