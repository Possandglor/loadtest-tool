package models

import (
	"time"
)

// RPSStep шаг изменения RPS
type RPSStep struct {
	RPS      int `json:"rps"`      // количество запросов в секунду
	Duration int `json:"duration"` // длительность в секундах
}

// Extractor извлечение данных из ответа
type Extractor struct {
	Name     string `json:"name"`
	JSONPath string `json:"json_path"`
	Regex    string `json:"regex"`
	Header   string `json:"header"`
}

// ScenarioStep шаг сценария
type ScenarioStep struct {
	Order      int               `json:"order"`
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	Extractors []Extractor       `json:"extractors,omitempty"`
}

// WeightedRequest запрос с весом для случайного выбора
type WeightedRequest struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Weight  int               `json:"weight"`
}

// TestConfig конфигурация нагрузочного теста
type TestConfig struct {
	ID               string            `json:"id" db:"id"`
	Name             string            `json:"name" db:"name"`
	URL              string            `json:"url" db:"url"`
	Method           string            `json:"method" db:"method"`
	Headers          map[string]string `json:"headers" db:"headers"`
	Body             string            `json:"body" db:"body"`
	BodyVariants     []string          `json:"body_variants,omitempty" db:"body_variants"`
	RPS              int               `json:"rps" db:"rps"`
	Duration         int               `json:"duration" db:"duration"`
	RPSSteps         []RPSStep         `json:"rps_steps,omitempty" db:"rps_steps"`
	TokenConfig      *TokenConfig      `json:"token_config,omitempty" db:"token_config"`
	IsSequential     bool              `json:"is_sequential" db:"is_sequential"`
	Steps            []ScenarioStep    `json:"steps,omitempty" db:"steps"`
	IsRandom         bool              `json:"is_random" db:"is_random"`
	WeightedRequests []WeightedRequest `json:"weighted_requests,omitempty" db:"weighted_requests"`
	CreatedAt        time.Time         `json:"created_at" db:"created_at"`
}

// TokenConfig конфигурация для получения токенов
type TokenConfig struct {
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	TokenPath  string            `json:"token_path"`  // JSON path для извлечения токена
	HeaderName string            `json:"header_name"` // имя заголовка для токена
	CacheTTL   int               `json:"cache_ttl"`   // TTL кеша в секундах
}

// TestResult результат одного запроса
type TestResult struct {
	ID           string    `json:"id" db:"id"`
	TestID       string    `json:"test_id" db:"test_id"`
	StatusCode   int       `json:"status_code" db:"status_code"`
	Duration     int64     `json:"duration" db:"duration"` // миллисекунды
	Error        string    `json:"error,omitempty" db:"error"`
	RequestBody  string    `json:"request_body,omitempty" db:"request_body"`
	ResponseBody string    `json:"response_body,omitempty" db:"response_body"`
	Timestamp    time.Time `json:"timestamp" db:"timestamp"`
}

// TestSession сессия тестирования
type TestSession struct {
	ID        string    `json:"id" db:"id"`
	TestID    string    `json:"test_id" db:"test_id"`
	Status    string    `json:"status" db:"status"` // running, stopped, completed
	StartedAt time.Time `json:"started_at" db:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty" db:"ended_at"`
}

// MetricsSnapshot снимок метрик за секунду
type MetricsSnapshot struct {
	Timestamp    time.Time         `json:"timestamp"`
	RPS          int               `json:"rps"`
	AvgDuration  float64           `json:"avg_duration"`
	P50Duration  float64           `json:"p50_duration"`
	P95Duration  float64           `json:"p95_duration"`
	P99Duration  float64           `json:"p99_duration"`
	StatusCodes  map[int]int       `json:"status_codes"`
	ErrorRate    float64           `json:"error_rate"`
	LastError    string            `json:"last_error,omitempty"`
}

// WSMessage сообщение для WebSocket
type WSMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}
