package server

import (
	"encoding/json"
	"loadtest-tool/database"
	"loadtest-tool/models"
	"loadtest-tool/services"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

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

	server := &Server{
		db:     db,
		engine: services.NewLoadTestEngine(),
		hub:    hub,
	}

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

	// Статические файлы
	r.Static("/static", "./web/static")
	r.LoadHTMLGlob("web/templates/*")

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
		api.GET("/results/:testId", s.getResults)
		api.POST("/export", s.exportConfigs)
		api.POST("/import", s.importConfigs)
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
	// TODO: Implement delete
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

	c.JSON(http.StatusOK, gin.H{"session_id": sessionID})
}

func (s *Server) stopTest(c *gin.Context) {
	sessionID := c.Param("id")
	
	if err := s.engine.StopTest(sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

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
