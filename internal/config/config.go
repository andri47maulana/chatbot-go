package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	// HTTP server
	Port            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration

	// Worker pool
	WorkerCount   int
	JobQueueSize  int

	// MySQL
	DBDriver   string
	DBHost     string
	DBPort     string
	DBName     string
	DBUser     string
	DBPassword string
	DBMaxOpen  int
	DBMaxIdle  int

	// Redis
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// OpenAI
	OpenAIAPIKey string
	OpenAIModel  string // default: gpt-4o-mini

	// WAHA (WhatsApp API)
	WAHABaseURL  string
	WAHAAPIKey   string
	WAHASession  string // default: default

	// RAG Backend
	RAGBaseURL string
	RAGAPIKey  string

	// Bot config
	BotLinkedDeviceID string // cached manually if needed
	WAHAAutoRestart   bool
	DedupeWindowMin   int // minutes to keep processed message IDs

	// App
	AppEnv   string
	LogLevel string
}

// Load reads config from environment variables.
// Call os.Setenv before this (or use godotenv.Load in main).
func Load() (*Config, error) {
	c := &Config{
		Port:              getEnv("APP_PORT", "9000"),
		ReadTimeout:       getDuration("HTTP_READ_TIMEOUT", 30*time.Second),
		WriteTimeout:      getDuration("HTTP_WRITE_TIMEOUT", 120*time.Second),
		ShutdownTimeout:   getDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
		WorkerCount:       getInt("WORKER_COUNT", 20),
		JobQueueSize:      getInt("JOB_QUEUE_SIZE", 500),
		DBDriver:          getEnv("DB_DRIVER", "mysql"),
		DBHost:            getEnv("DB_HOST", "127.0.0.1"),
		DBPort:            getEnv("DB_PORT", "3306"),
		DBName:            getEnv("DB_DATABASE", "aigri"),
		DBUser:            getEnv("DB_USERNAME", "root"),
		DBPassword:        getEnv("DB_PASSWORD", ""),
		DBMaxOpen:         getInt("DB_MAX_OPEN", 25),
		DBMaxIdle:         getInt("DB_MAX_IDLE", 10),
		RedisAddr:         getEnv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:     getEnv("REDIS_PASSWORD", ""),
		RedisDB:           getInt("REDIS_DB", 0),
		OpenAIAPIKey:      getEnv("OPENAI_API_KEY", ""),
		OpenAIModel:       getEnv("OPENAI_MODEL", "gpt-4o-mini"),
		WAHABaseURL:       getEnv("WAHA_BASE_URL", "http://localhost:3000"),
		WAHAAPIKey:        getEnv("WAHA_API_KEY", ""),
		WAHASession:       getEnv("WAHA_SESSION", "default"),
		RAGBaseURL:        getEnv("RAG_BASE_URL", "http://localhost:5002"),
		RAGAPIKey:         getEnv("RAG_API_KEY", "1245"),
		BotLinkedDeviceID: getEnv("BOT_LINKED_DEVICE_ID", ""),
		WAHAAutoRestart:   getBool("WAHA_AUTO_RESTART", false),
		DedupeWindowMin:   getInt("DEDUPE_WINDOW_MIN", 5),
		AppEnv:           getEnv("APP_ENV", "production"),
		LogLevel:         getEnv("LOG_LEVEL", "info"),
	}

	if c.OpenAIAPIKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is required")
	}

	return c, nil
}

// DSN returns the MySQL DSN string.
func (c *Config) DSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?parseTime=true&loc=Local&charset=utf8mb4",
		c.DBUser, c.DBPassword, c.DBHost, c.DBPort, c.DBName,
	)
}

// ---- helpers ----------------------------------------------------------------

func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
