package server

import (
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"loadtest-tool/database"
	"loadtest-tool/models"
	"loadtest-tool/services"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

//go:embed web/templates/*
var templatesFS embed.FS

//go:embed web/static/*
var staticFS embed.FS

type Server struct {
	db     *database.DB
	engine *services.LoadTestEngine
	hub    *WSHub
}

type WSHub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan models.WSMessage
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func NewServer(db *database.DB) *Server {
	hub := &WSHub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan models.WSMessage),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}

	engine := services.NewLoadTestEngine()
	
	server := &Server{
		db:     db,
		engine: engine,
		hub:    hub,
	}

	// Устанавливаем callback для завершения сессий
	engine.SetOnSessionComplete(func(sessionID string, status string) {
		db.UpdateTestSession(sessionID, status, time.Now())
	})

	go hub.run()
	go server.handleResults()
	go server.handleMetrics()

	return server
}

func (h *WSHub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.Close()
			}

		case message := <-h.broadcast:
			data, _ := json.Marshal(message)
			for client := range h.clients {
				err := client.WriteMessage(websocket.TextMessage, data)
				if err != nil {
					delete(h.clients, client)
					client.Close()
				}
			}
		}
	}
}

func (s *Server) handleResults() {
	for result := range s.engine.GetResultsChan() {
		s.db.SaveTestResult(&result)
	}
}

func (s *Server) handleMetrics() {
	for metrics := range s.engine.GetMetricsChan() {
		s.hub.broadcast <- models.WSMessage{
			Type: "metrics",
			Data: metrics,
		}
	}
}

func (s *Server) SetupRoutes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Загружаем шаблоны из embed
	tmpl := template.Must(template.New("").ParseFS(templatesFS, "web/templates/*"))
	r.SetHTMLTemplate(tmpl)

	// Статические файлы из embed
	staticFiles, _ := fs.Sub(staticFS, "web/static")
	r.StaticFS("/static", http.FS(staticFiles))

	// Главная страница
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", nil)
	})

	// API routes
	api := r.Group("/api")
	{
		api.GET("/configs", s.getConfigs)
		api.GET("/configs/:id", s.getConfig)
		api.POST("/configs", s.createConfig)
		api.PUT("/configs/:id", s.updateConfig)
		api.DELETE("/configs/:id", s.deleteConfig)
		api.POST("/configs/:id/start", s.startTest)
		api.POST("/sessions/:id/stop", s.stopTest)
		api.GET("/sessions/active", s.getActiveSessions)
		api.GET("/sessions/:testId", s.getSessions)
		api.GET("/results/:testId", s.getResults)
		api.GET("/session-results/:sessionId", s.getSessionResults)
		api.DELETE("/sessions/:id", s.deleteSession)
		api.DELETE("/sessions", s.deleteAllSessions)
		api.POST("/export", s.exportConfigs)
		api.POST("/import", s.importConfigs)
		api.GET("/session-results/:sessionId/csv", s.exportSessionCSV)
		api.GET("/compare-sessions", s.compareSessions)
	}

	// WebSocket
	r.GET("/ws", s.handleWebSocket)

	return r
}

func (s *Server) getConfigs(c *gin.Context) {
	configs, err := s.db.GetTestConfigs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, configs)
}

func (s *Server) getConfig(c *gin.Context) {
	configID := c.Param("id")
	config, err := s.db.GetTestConfig(configID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "config not found"})
		return
	}
	c.JSON(http.StatusOK, config)
}

func (s *Server) createConfig(c *gin.Context) {
	var config models.TestConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	config.ID = uuid.New().String()
	config.CreatedAt = time.Now()

	if err := s.db.SaveTestConfig(&config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, config)
}

func (s *Server) updateConfig(c *gin.Context) {
	var config models.TestConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	config.ID = c.Param("id")
	if err := s.db.SaveTestConfig(&config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, config)
}

func (s *Server) deleteConfig(c *gin.Context) {
	configID := c.Param("id")
	if err := s.db.DeleteTestConfig(configID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (s *Server) startTest(c *gin.Context) {
	configID := c.Param("id")
	
	configs, err := s.db.GetTestConfigs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var config *models.TestConfig
	for _, cfg := range configs {
		if cfg.ID == configID {
			config = &cfg
			break
		}
	}

	if config == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "config not found"})
		return
	}

	sessionID, err := s.engine.StartTest(config)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Сохраняем сессию в БД
	s.db.SaveTestSession(sessionID, configID)

	c.JSON(http.StatusOK, gin.H{"session_id": sessionID})
}

func (s *Server) stopTest(c *gin.Context) {
	sessionID := c.Param("id")
	
	if err := s.engine.StopTest(sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Обновляем статус сессии в БД
	s.db.UpdateTestSession(sessionID, "stopped", time.Now())

	c.JSON(http.StatusOK, gin.H{"message": "stopped"})
}

func (s *Server) getResults(c *gin.Context) {
	testID := c.Param("testId")
	
	// Parse since parameter
	sinceStr := c.Query("since")
	since := time.Now().Add(-1 * time.Hour) // default: last hour
	
	if sinceStr != "" {
		parsedTime, err := time.Parse(time.RFC3339, sinceStr)
		if err == nil {
			since = parsedTime
		}
	}

	results, err := s.db.GetTestResults(testID, since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, results)
}

func (s *Server) getSessions(c *gin.Context) {
	testID := c.Param("testId")
	
	sessions, err := s.db.GetTestSessions(testID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, sessions)
}

func (s *Server) getActiveSessions(c *gin.Context) {
	sessions, err := s.db.GetActiveSessions()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, sessions)
}

func (s *Server) getSessionResults(c *gin.Context) {
	sessionID := c.Param("sessionId")
	
	session, err := s.db.GetTestSession(sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	testID := session["test_id"].(string)
	startedAt := session["started_at"].(time.Time)

	// Получаем результаты за период сессии
	results, err := s.db.GetTestResults(testID, startedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Фильтруем результаты по времени сессии
	sessionResults := []models.TestResult{}
	
	for _, r := range results {
		if r.Timestamp.After(startedAt) || r.Timestamp.Equal(startedAt) {
			if endedAt, hasEnded := session["ended_at"].(time.Time); hasEnded {
				if r.Timestamp.Before(endedAt) || r.Timestamp.Equal(endedAt) {
					sessionResults = append(sessionResults, r)
				}
			} else {
				sessionResults = append(sessionResults, r)
			}
		}
	}

	c.JSON(http.StatusOK, sessionResults)
}

func (s *Server) deleteSession(c *gin.Context) {
	sessionID := c.Param("id")
	if err := s.db.DeleteTestSession(sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (s *Server) deleteAllSessions(c *gin.Context) {
	testID := c.Query("test_id")
	if err := s.db.DeleteAllSessions(testID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (s *Server) exportConfigs(c *gin.Context) {
	configs, err := s.db.GetTestConfigs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Header("Content-Disposition", "attachment; filename=loadtest-configs.json")
	c.JSON(http.StatusOK, configs)
}

func (s *Server) importConfigs(c *gin.Context) {
	var configs []models.TestConfig
	if err := c.ShouldBindJSON(&configs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	for _, config := range configs {
		config.ID = uuid.New().String()
		config.CreatedAt = time.Now()
		s.db.SaveTestConfig(&config)
	}

	c.JSON(http.StatusOK, gin.H{"imported": len(configs)})
}

func (s *Server) exportSessionCSV(c *gin.Context) {
	sessionID := c.Param("sessionId")

	session, err := s.db.GetTestSession(sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	testID := session["test_id"].(string)
	startedAt := session["started_at"].(time.Time)

	results, err := s.db.GetTestResults(testID, startedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=session-%s.csv", sessionID[:8]))

	w := csv.NewWriter(c.Writer)
	w.Write([]string{"timestamp", "status_code", "duration_ms", "error", "request_body", "response_body"})

	for _, r := range results {
		if r.Timestamp.Before(startedAt) {
			continue
		}
		if endedAt, ok := session["ended_at"].(time.Time); ok && r.Timestamp.After(endedAt) {
			continue
		}
		w.Write([]string{
			r.Timestamp.Format(time.RFC3339),
			fmt.Sprintf("%d", r.StatusCode),
			fmt.Sprintf("%d", r.Duration),
			r.Error,
			r.RequestBody,
			r.ResponseBody,
		})
	}
	w.Flush()
}

func (s *Server) compareSessions(c *gin.Context) {
	ids := strings.Split(c.Query("ids"), ",")
	if len(ids) < 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "need at least 2 session ids"})
		return
	}

	type SessionSummary struct {
		SessionID    string             `json:"session_id"`
		TotalReqs    int                `json:"total_requests"`
		AvgDuration  float64            `json:"avg_duration"`
		MinDuration  int64              `json:"min_duration"`
		MaxDuration  int64              `json:"max_duration"`
		P50Duration  float64            `json:"p50_duration"`
		P95Duration  float64            `json:"p95_duration"`
		P99Duration  float64            `json:"p99_duration"`
		ErrorRate    float64            `json:"error_rate"`
		Throughput   float64            `json:"throughput"`
		StatusCodes  map[int]int        `json:"status_codes"`
		DurationSec  float64            `json:"duration_sec"`
	}

	var summaries []SessionSummary

	for _, sid := range ids {
		sid = strings.TrimSpace(sid)
		sess, err := s.db.GetTestSession(sid)
		if err != nil {
			continue
		}

		testID := sess["test_id"].(string)
		startedAt := sess["started_at"].(time.Time)
		results, err := s.db.GetTestResults(testID, startedAt)
		if err != nil {
			continue
		}

		var filtered []models.TestResult
		for _, r := range results {
			if r.Timestamp.Before(startedAt) {
				continue
			}
			if endedAt, ok := sess["ended_at"].(time.Time); ok && r.Timestamp.After(endedAt) {
				continue
			}
			filtered = append(filtered, r)
		}

		if len(filtered) == 0 {
			continue
		}

		summary := SessionSummary{
			SessionID:   sid,
			TotalReqs:   len(filtered),
			StatusCodes: make(map[int]int),
		}

		var totalDur int64
		var errors int
		var minDur, maxDur int64
		minDur = filtered[0].Duration

		for _, r := range filtered {
			totalDur += r.Duration
			summary.StatusCodes[r.StatusCode]++
			if r.Duration < minDur {
				minDur = r.Duration
			}
			if r.Duration > maxDur {
				maxDur = r.Duration
			}
			if r.Error != "" || r.StatusCode >= 400 {
				errors++
			}
		}

		summary.AvgDuration = float64(totalDur) / float64(len(filtered))
		summary.MinDuration = minDur
		summary.MaxDuration = maxDur
		summary.ErrorRate = float64(errors) / float64(len(filtered)) * 100

		endedAt := time.Now()
		if ea, ok := sess["ended_at"].(time.Time); ok {
			endedAt = ea
		}
		summary.DurationSec = endedAt.Sub(startedAt).Seconds()
		if summary.DurationSec > 0 {
			summary.Throughput = float64(len(filtered)-errors) / summary.DurationSec
		}

		// Percentiles
		durations := make([]int64, len(filtered))
		for i, r := range filtered {
			durations[i] = r.Duration
		}
		// Simple sort for percentiles
		for i := 1; i < len(durations); i++ {
			key := durations[i]
			j := i - 1
			for j >= 0 && durations[j] > key {
				durations[j+1] = durations[j]
				j--
			}
			durations[j+1] = key
		}
		p := func(pct float64) float64 {
			idx := int(float64(len(durations)) * pct / 100)
			if idx >= len(durations) {
				idx = len(durations) - 1
			}
			return float64(durations[idx])
		}
		summary.P50Duration = p(50)
		summary.P95Duration = p(95)
		summary.P99Duration = p(99)

		summaries = append(summaries, summary)
	}

	c.JSON(http.StatusOK, summaries)
}

func (s *Server) handleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	s.hub.register <- conn

	go func() {
		defer func() {
			s.hub.unregister <- conn
		}()

		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
		}
	}()
}
