package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"loadtest-tool/models"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type LoadTestEngine struct {
	tokenService      *TokenService
	resultsChan       chan models.TestResult
	metricsChan       chan models.MetricsSnapshot
	sessions          map[string]*TestSession
	mutex             sync.RWMutex
	onSessionComplete func(sessionID string, status string) // callback при завершении сессии
}

type TestSession struct {
	ID          string
	Config      *models.TestConfig
	Cancel      context.CancelFunc
	IsActive    bool
	Variables   map[string]string
	VarMutex    sync.RWMutex
	ResultsChan chan models.TestResult
	InFlight    int64 // atomic counter для запросов "в полёте"
}

func NewLoadTestEngine() *LoadTestEngine {
	rand.Seed(time.Now().UnixNano()) // Инициализация генератора случайных чисел
	return &LoadTestEngine{
		tokenService: NewTokenService(),
		resultsChan:  make(chan models.TestResult, 1000),
		metricsChan:  make(chan models.MetricsSnapshot, 100),
		sessions:     make(map[string]*TestSession),
	}
}

func (lte *LoadTestEngine) StartTest(config *models.TestConfig) (string, error) {
	sessionID := uuid.New().String()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.Duration)*time.Second)

	session := &TestSession{
		ID:          sessionID,
		Config:      config,
		Cancel:      cancel,
		IsActive:    true,
		Variables:   make(map[string]string),
		ResultsChan: make(chan models.TestResult, 100),
	}

	lte.mutex.Lock()
	lte.sessions[sessionID] = session
	lte.mutex.Unlock()

	go lte.runTest(ctx, session)
	go lte.collectMetrics(sessionID)
	
	// Запускаем обновление токенов каждую минуту
	if config.TokenConfig != nil {
		go lte.updateTokensPeriodically(ctx, session)
	}

	return sessionID, nil
}

func (lte *LoadTestEngine) StopTest(sessionID string) error {
	lte.mutex.Lock()
	defer lte.mutex.Unlock()

	session, exists := lte.sessions[sessionID]
	if !exists {
		return nil
	}

	session.Cancel()
	session.IsActive = false
	
	if lte.onSessionComplete != nil {
		go lte.onSessionComplete(sessionID, "stopped")
	}
	
	return nil
}

func (lte *LoadTestEngine) runTest(ctx context.Context, session *TestSession) {
	defer func() {
		lte.mutex.Lock()
		session.IsActive = false
		lte.mutex.Unlock()
		
		if lte.onSessionComplete != nil {
			lte.onSessionComplete(session.ID, "completed")
		}
	}()

	config := session.Config
	
	timeout := 30 * time.Second
	if config.TimeoutMs > 0 {
		timeout = time.Duration(config.TimeoutMs) * time.Millisecond
	}
	
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	if config.IsSequential {
		lte.runSequentialScenario(ctx, session, client)
	} else if config.IsRandom {
		lte.runRandomTest(ctx, session, client)
	} else if len(config.RPSSteps) > 0 {
		lte.runDynamicRPSTest(ctx, session, client)
	} else {
		lte.runStaticRPSTest(ctx, session, client, config.RPS)
	}
}

func (lte *LoadTestEngine) runDynamicRPSTest(ctx context.Context, session *TestSession, client *http.Client) {
	for _, step := range session.Config.RPSSteps {
		rpsStart := step.RPS
		rpsEnd := step.RPSEnd
		if rpsEnd <= 0 {
			rpsEnd = rpsStart
		}

		stepDuration := time.Duration(step.Duration) * time.Second
		stepCtx, stepCancel := context.WithTimeout(ctx, stepDuration)

		if rpsStart == rpsEnd {
			go lte.runStaticRPSTest(stepCtx, session, client, rpsStart)
		} else {
			go lte.runRampRPSTest(stepCtx, session, client, rpsStart, rpsEnd, stepDuration)
		}

		select {
		case <-stepCtx.Done():
			stepCancel()
		case <-ctx.Done():
			stepCancel()
			return
		}
	}
}

func (lte *LoadTestEngine) runRampRPSTest(ctx context.Context, session *TestSession, client *http.Client, rpsStart, rpsEnd int, duration time.Duration) {
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		elapsed := time.Since(startTime)
		progress := float64(elapsed) / float64(duration)
		if progress > 1 {
			progress = 1
		}

		currentRPS := float64(rpsStart) + (float64(rpsEnd)-float64(rpsStart))*progress
		if currentRPS < 1 {
			currentRPS = 1
		}

		interval := time.Second / time.Duration(currentRPS)
		go lte.executeRequest(client, session)

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (lte *LoadTestEngine) runStaticRPSTest(ctx context.Context, session *TestSession, client *http.Client, rps int) {
	if rps <= 0 {
		return
	}
	
	interval := time.Second / time.Duration(rps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			go lte.executeRequest(client, session)
		}
	}
}

func (lte *LoadTestEngine) updateTokensPeriodically(ctx context.Context, session *TestSession) {
	// Сразу получаем токен
	lte.updateSessionToken(session)
	
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lte.updateSessionToken(session)
		}
	}
}

func (lte *LoadTestEngine) updateSessionToken(session *TestSession) {
	if session.Config.TokenConfig == nil {
		return
	}

	token, err := lte.tokenService.GetToken(session.Config.TokenConfig)
	if err != nil {
		return
	}

	session.VarMutex.Lock()
	session.Variables["sid"] = token
	session.Variables["token"] = token
	session.VarMutex.Unlock()
}

func (lte *LoadTestEngine) executeRequest(client *http.Client, session *TestSession) {
	atomic.AddInt64(&session.InFlight, 1)
	defer atomic.AddInt64(&session.InFlight, -1)

	start := time.Now()
	config := session.Config
	result := models.TestResult{
		ID:        uuid.New().String(),
		TestID:    config.ID,
		Timestamp: start,
	}

	lte.mutex.RLock()
	isActive := session.IsActive
	lte.mutex.RUnlock()
	
	if !isActive {
		return
	}

	var body io.Reader
	var bodyText string
	
	if len(config.BodyVariants) > 0 {
		bodyText = config.BodyVariants[rand.Intn(len(config.BodyVariants))]
	} else if config.Body != "" {
		bodyText = config.Body
	}
	
	if bodyText != "" {
		processedBody := lte.replaceVariables(bodyText, session)
		body = bytes.NewBufferString(processedBody)
		result.RequestBody = processedBody
	}

	processedURL := lte.replaceVariables(config.URL, session)
	req, err := http.NewRequest(config.Method, processedURL, body)
	if err != nil {
		result.Error = err.Error()
		lte.safeChannelSend(session, result)
		return
	}

	for key, value := range config.Headers {
		processedValue := lte.replaceVariables(value, session)
		req.Header.Set(key, processedValue)
	}

	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start).Milliseconds()
		result.StatusCode = 0
		lte.safeChannelSend(session, result)
		return
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		result.Error = readErr.Error()
	} else {
		result.ResponseBody = string(responseBody)
	}

	result.StatusCode = resp.StatusCode
	result.Duration = time.Since(start).Milliseconds()

	// Проверяем assertions
	if len(config.Assertions) > 0 && result.Error == "" {
		if failMsg := lte.checkAssertions(config.Assertions, result); failMsg != "" {
			result.Error = "ASSERTION: " + failMsg
		}
	}

	lte.safeChannelSend(session, result)
}

func (lte *LoadTestEngine) safeChannelSend(session *TestSession, result models.TestResult) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	
	select {
	case session.ResultsChan <- result:
	default:
	}
}

func (lte *LoadTestEngine) checkAssertions(assertions []models.Assertion, result models.TestResult) string {
	for _, a := range assertions {
		switch a.Type {
		case "status_code":
			expected, _ := strconv.Atoi(a.Value)
			switch a.Operator {
			case "eq":
				if result.StatusCode != expected {
					return "status_code " + strconv.Itoa(result.StatusCode) + " != " + a.Value
				}
			case "neq":
				if result.StatusCode == expected {
					return "status_code == " + a.Value
				}
			}
		case "body_contains":
			contains := strings.Contains(result.ResponseBody, a.Value)
			if a.Operator == "contains" && !contains {
				return "body does not contain '" + a.Value + "'"
			}
			if a.Operator == "not_contains" && contains {
				return "body contains '" + a.Value + "'"
			}
		case "body_json_path":
			actual := lte.extractJSONPath(result.ResponseBody, a.JSONPath)
			switch a.Operator {
			case "eq":
				if actual != a.Value {
					return a.JSONPath + " = '" + actual + "' != '" + a.Value + "'"
				}
			case "contains":
				if !strings.Contains(actual, a.Value) {
					return a.JSONPath + " does not contain '" + a.Value + "'"
				}
			}
		}
	}
	return ""
}

func (lte *LoadTestEngine) replaceVariables(text string, session *TestSession) string {
	session.VarMutex.RLock()
	defer session.VarMutex.RUnlock()

	result := text
	
	// Заменяем обычные переменные
	for key, value := range session.Variables {
		placeholder := "{{" + key + "}}"
		result = strings.ReplaceAll(result, placeholder, value)
	}
	
	// Обрабатываем динамические функции
	result = lte.processDynamicFunctions(result)
	
	return result
}

func (lte *LoadTestEngine) processDynamicFunctions(text string) string {
	// {{random:value1,value2,value3}} - случайный выбор из списка
	randomRegex := regexp.MustCompile(`\{\{random:([^}]+)\}\}`)
	text = randomRegex.ReplaceAllStringFunc(text, func(match string) string {
		values := strings.Split(randomRegex.FindStringSubmatch(match)[1], ",")
		for i, v := range values {
			values[i] = strings.TrimSpace(v)
		}
		return values[rand.Intn(len(values))]
	})
	
	// {{randomJson:{"key":"val1"},{"key":"val2"}}} - случайный выбор JSON объектов
	randomJsonRegex := regexp.MustCompile(`\{\{randomJson:([^}]+(?:\}[^}]*)*)\}\}`)
	text = randomJsonRegex.ReplaceAllStringFunc(text, func(match string) string {
		jsonContent := randomJsonRegex.FindStringSubmatch(match)[1]
		
		// Разбираем JSON объекты, учитывая вложенные скобки
		var jsonObjects []string
		var current strings.Builder
		braceCount := 0
		inString := false
		escaped := false
		
		for _, char := range jsonContent {
			if escaped {
				escaped = false
				current.WriteRune(char)
				continue
			}
			
			if char == '\\' {
				escaped = true
				current.WriteRune(char)
				continue
			}
			
			if char == '"' {
				inString = !inString
				current.WriteRune(char)
				continue
			}
			
			if !inString {
				if char == '{' {
					if braceCount == 0 && current.Len() > 0 {
						// Начинаем новый объект
						jsonObjects = append(jsonObjects, strings.TrimSpace(current.String()))
						current.Reset()
					}
					braceCount++
				} else if char == '}' {
					braceCount--
				} else if char == ',' && braceCount == 0 {
					// Разделитель между объектами
					jsonObjects = append(jsonObjects, strings.TrimSpace(current.String()))
					current.Reset()
					continue
				}
			}
			
			current.WriteRune(char)
		}
		
		// Добавляем последний объект
		if current.Len() > 0 {
			jsonObjects = append(jsonObjects, strings.TrimSpace(current.String()))
		}
		
		if len(jsonObjects) > 0 {
			return jsonObjects[rand.Intn(len(jsonObjects))]
		}
		return match // Возвращаем исходный текст если не удалось распарсить
	})
	
	// {{randomInt:min,max}} - случайное число в диапазоне
	randomIntRegex := regexp.MustCompile(`\{\{randomInt:(\d+),(\d+)\}\}`)
	text = randomIntRegex.ReplaceAllStringFunc(text, func(match string) string {
		matches := randomIntRegex.FindStringSubmatch(match)
		min, _ := strconv.Atoi(matches[1])
		max, _ := strconv.Atoi(matches[2])
		return strconv.Itoa(rand.Intn(max-min+1) + min)
	})
	
	// {{uuid}} - генерация UUID
	uuidRegex := regexp.MustCompile(`\{\{uuid\}\}`)
	text = uuidRegex.ReplaceAllStringFunc(text, func(match string) string {
		return uuid.New().String()
	})
	
	// {{timestamp}} - текущий timestamp
	timestampRegex := regexp.MustCompile(`\{\{timestamp\}\}`)
	text = timestampRegex.ReplaceAllStringFunc(text, func(match string) string {
		return strconv.FormatInt(time.Now().Unix(), 10)
	})
	
	// {{randomString:length}} - случайная строка заданной длины
	randomStringRegex := regexp.MustCompile(`\{\{randomString:(\d+)\}\}`)
	text = randomStringRegex.ReplaceAllStringFunc(text, func(match string) string {
		matches := randomStringRegex.FindStringSubmatch(match)
		length, _ := strconv.Atoi(matches[1])
		return lte.generateRandomString(length)
	})
	
	// {{randomEmail}} - случайный email
	randomEmailRegex := regexp.MustCompile(`\{\{randomEmail\}\}`)
	text = randomEmailRegex.ReplaceAllStringFunc(text, func(match string) string {
		domains := []string{"gmail.com", "yahoo.com", "hotmail.com", "example.com"}
		username := lte.generateRandomString(8)
		domain := domains[rand.Intn(len(domains))]
		return strings.ToLower(username) + "@" + domain
	})
	
	return text
}

func (lte *LoadTestEngine) generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

func (lte *LoadTestEngine) collectMetrics(sessionID string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	results := make([]models.TestResult, 0)
	startTime := time.Now()
	
	lte.mutex.RLock()
	session, exists := lte.sessions[sessionID]
	lte.mutex.RUnlock()
	
	if !exists {
		return
	}
	
	defer func() {
		defer func() {
			if r := recover(); r != nil {
			}
		}()
		close(session.ResultsChan)
	}()
	
	for {
		select {
		case <-ticker.C:
			if len(results) > 0 {
				snapshot := lte.calculateMetrics(results, session)
				lte.metricsChan <- snapshot
				
				// Auto-stop check
				if lte.shouldAutoStop(session, snapshot, startTime) {
					lte.mutex.Lock()
					session.IsActive = false
					session.Cancel()
					lte.mutex.Unlock()
					if lte.onSessionComplete != nil {
						lte.onSessionComplete(sessionID, "auto_stopped")
					}
					// Отправляем WS сообщение об авто-стопе
					lte.metricsChan <- models.MetricsSnapshot{
						Timestamp: time.Now(),
						LastError: "AUTO_STOP: " + lte.autoStopReason(session, snapshot),
					}
					return
				}
				
				results = results[:0]
			}

			lte.mutex.RLock()
			isActive := session.IsActive
			lte.mutex.RUnlock()

			if !isActive {
				return
			}

		case result, ok := <-session.ResultsChan:
			if !ok {
				return
			}
			results = append(results, result)
			
			select {
			case lte.resultsChan <- result:
			default:
			}
		}
	}
}

func (lte *LoadTestEngine) shouldAutoStop(session *TestSession, snapshot models.MetricsSnapshot, startTime time.Time) bool {
	cfg := session.Config.AutoStop
	if cfg == nil || !cfg.Enabled {
		return false
	}
	if cfg.CheckAfterSec > 0 && time.Since(startTime).Seconds() < float64(cfg.CheckAfterSec) {
		return false
	}
	if cfg.MaxErrorRate > 0 && snapshot.ErrorRate > cfg.MaxErrorRate {
		return true
	}
	if cfg.MaxAvgResponseMs > 0 && snapshot.AvgDuration > float64(cfg.MaxAvgResponseMs) {
		return true
	}
	return false
}

func (lte *LoadTestEngine) autoStopReason(session *TestSession, snapshot models.MetricsSnapshot) string {
	cfg := session.Config.AutoStop
	if cfg.MaxErrorRate > 0 && snapshot.ErrorRate > cfg.MaxErrorRate {
		return "error rate " + strconv.FormatFloat(snapshot.ErrorRate, 'f', 1, 64) + "% > " + strconv.FormatFloat(cfg.MaxErrorRate, 'f', 1, 64) + "%"
	}
	if cfg.MaxAvgResponseMs > 0 && snapshot.AvgDuration > float64(cfg.MaxAvgResponseMs) {
		return "avg response " + strconv.FormatFloat(snapshot.AvgDuration, 'f', 0, 64) + "ms > " + strconv.FormatInt(cfg.MaxAvgResponseMs, 10) + "ms"
	}
	return "unknown"
}

func (lte *LoadTestEngine) calculateMetrics(results []models.TestResult, session *TestSession) models.MetricsSnapshot {
	snapshot := models.MetricsSnapshot{
		Timestamp:   time.Now(),
		RPS:         len(results),
		StatusCodes: make(map[int]int),
		InFlight:    int(atomic.LoadInt64(&session.InFlight)),
	}

	if len(results) == 0 {
		return snapshot
	}

	var totalDuration int64
	var errorCount int
	var assertionFails int
	var lastError string
	var minDur, maxDur int64
	var throughput int
	durations := make([]int64, 0, len(results))

	minDur = results[0].Duration

	for _, result := range results {
		totalDuration += result.Duration
		durations = append(durations, result.Duration)
		snapshot.StatusCodes[result.StatusCode]++
		
		if result.Duration < minDur {
			minDur = result.Duration
		}
		if result.Duration > maxDur {
			maxDur = result.Duration
		}
		
		if result.StatusCode > 0 && result.StatusCode < 400 {
			throughput++
		}
		
		if result.Error != "" || result.StatusCode >= 400 {
			errorCount++
			if result.Error != "" {
				if strings.HasPrefix(result.Error, "ASSERTION:") {
					assertionFails++
				}
				lastError = result.Error
			}
		}
	}

	snapshot.Throughput = throughput
	snapshot.MinDuration = float64(minDur)
	snapshot.MaxDuration = float64(maxDur)
	snapshot.AvgDuration = float64(totalDuration) / float64(len(results))
	snapshot.ErrorRate = float64(errorCount) / float64(len(results)) * 100
	snapshot.AssertionFails = assertionFails
	snapshot.LastError = lastError

	snapshot.P50Duration = lte.calculatePercentile(durations, 50)
	snapshot.P95Duration = lte.calculatePercentile(durations, 95)
	snapshot.P99Duration = lte.calculatePercentile(durations, 99)

	return snapshot
}

func (lte *LoadTestEngine) calculatePercentile(durations []int64, percentile float64) float64 {
	if len(durations) == 0 {
		return 0
	}

	// Сортируем копию
	sorted := make([]int64, len(durations))
	copy(sorted, durations)
	
	// Простая сортировка вставками (для небольших массивов быстрее)
	for i := 1; i < len(sorted); i++ {
		key := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > key {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = key
	}

	index := int(float64(len(sorted)) * percentile / 100.0)
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return float64(sorted[index])
}

func (lte *LoadTestEngine) GetResultsChan() <-chan models.TestResult {
	return lte.resultsChan
}

func (lte *LoadTestEngine) GetMetricsChan() <-chan models.MetricsSnapshot {
	return lte.metricsChan
}

func (lte *LoadTestEngine) SetOnSessionComplete(callback func(sessionID string, status string)) {
	lte.onSessionComplete = callback
}

func (lte *LoadTestEngine) runSequentialScenario(ctx context.Context, session *TestSession, client *http.Client) {
	if len(session.Config.Steps) == 0 {
		return
	}

	rps := session.Config.RPS
	if rps <= 0 {
		rps = 1
	}
	interval := time.Second / time.Duration(rps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			go lte.executeScenario(client, session)
		}
	}
}

func (lte *LoadTestEngine) executeScenario(client *http.Client, session *TestSession) {
	scenarioVars := make(map[string]string)

	for _, step := range session.Config.Steps {
		result := lte.executeStep(client, session, step, scenarioVars)

		for _, extractor := range step.Extractors {
			value := lte.extractValue(result.ResponseBody, extractor)
			scenarioVars[extractor.Name] = value
		}

		lte.safeChannelSend(session, result)

		if result.Error != "" || result.StatusCode >= 400 {
			return
		}

		// Think time между шагами
		if step.ThinkTime > 0 {
			time.Sleep(time.Duration(step.ThinkTime) * time.Millisecond)
		}
	}
}

func (lte *LoadTestEngine) executeStep(client *http.Client, session *TestSession, step models.ScenarioStep, scenarioVars map[string]string) models.TestResult {
	start := time.Now()
	result := models.TestResult{
		ID:        uuid.New().String(),
		TestID:    session.Config.ID,
		Timestamp: start,
	}

	var body io.Reader
	if step.Body != "" {
		processedBody := lte.replaceVariablesWithScenario(step.Body, session, scenarioVars)
		body = bytes.NewBufferString(processedBody)
		result.RequestBody = processedBody
	}

	processedURL := lte.replaceVariablesWithScenario(step.URL, session, scenarioVars)
	req, err := http.NewRequest(step.Method, processedURL, body)
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start).Milliseconds()
		return result
	}

	for key, value := range step.Headers {
		processedValue := lte.replaceVariablesWithScenario(value, session, scenarioVars)
		req.Header.Set(key, processedValue)
	}

	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start).Milliseconds()
		return result
	}
	defer resp.Body.Close()

	responseBody, _ := io.ReadAll(resp.Body)
	result.ResponseBody = string(responseBody)
	result.StatusCode = resp.StatusCode
	result.Duration = time.Since(start).Milliseconds()

	return result
}

func (lte *LoadTestEngine) replaceVariablesWithScenario(text string, session *TestSession, scenarioVars map[string]string) string {
	result := text

	for key, value := range scenarioVars {
		placeholder := "{{" + key + "}}"
		result = strings.ReplaceAll(result, placeholder, value)
	}

	session.VarMutex.RLock()
	for key, value := range session.Variables {
		placeholder := "{{" + key + "}}"
		result = strings.ReplaceAll(result, placeholder, value)
	}
	session.VarMutex.RUnlock()

	return lte.processDynamicFunctions(result)
}

func (lte *LoadTestEngine) extractValue(responseBody string, extractor models.Extractor) string {
	if extractor.JSONPath != "" {
		return lte.extractJSONPath(responseBody, extractor.JSONPath)
	}
	if extractor.Regex != "" {
		re := regexp.MustCompile(extractor.Regex)
		matches := re.FindStringSubmatch(responseBody)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (lte *LoadTestEngine) extractJSONPath(responseBody, jsonPath string) string {
	var data interface{}
	if err := json.Unmarshal([]byte(responseBody), &data); err != nil {
		return ""
	}

	path := strings.TrimPrefix(jsonPath, "$.")
	parts := strings.Split(path, ".")

	current := data
	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[part]
		default:
			return ""
		}
	}

	if current == nil {
		return ""
	}

	switch v := current.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func (lte *LoadTestEngine) runRandomTest(ctx context.Context, session *TestSession, client *http.Client) {
	if len(session.Config.WeightedRequests) == 0 {
		return
	}

	rps := session.Config.RPS
	if rps <= 0 {
		rps = 1
	}
	interval := time.Second / time.Duration(rps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			request := lte.selectWeightedRequest(session.Config.WeightedRequests)
			go lte.executeWeightedRequest(client, session, request)
		}
	}
}

func (lte *LoadTestEngine) selectWeightedRequest(requests []models.WeightedRequest) models.WeightedRequest {
	totalWeight := 0
	for _, r := range requests {
		totalWeight += r.Weight
	}

	if totalWeight == 0 {
		return requests[0]
	}

	random := rand.Intn(totalWeight)
	cumulative := 0

	for _, r := range requests {
		cumulative += r.Weight
		if random < cumulative {
			return r
		}
	}

	return requests[0]
}

func (lte *LoadTestEngine) executeWeightedRequest(client *http.Client, session *TestSession, request models.WeightedRequest) {
	atomic.AddInt64(&session.InFlight, 1)
	defer atomic.AddInt64(&session.InFlight, -1)

	start := time.Now()
	result := models.TestResult{
		ID:        uuid.New().String(),
		TestID:    session.Config.ID,
		Timestamp: start,
	}

	lte.mutex.RLock()
	isActive := session.IsActive
	lte.mutex.RUnlock()

	if !isActive {
		return
	}

	var body io.Reader
	if request.Body != "" {
		processedBody := lte.replaceVariables(request.Body, session)
		body = bytes.NewBufferString(processedBody)
		result.RequestBody = processedBody
	}

	processedURL := lte.replaceVariables(request.URL, session)
	req, err := http.NewRequest(request.Method, processedURL, body)
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start).Milliseconds()
		lte.safeChannelSend(session, result)
		return
	}

	for key, value := range request.Headers {
		processedValue := lte.replaceVariables(value, session)
		req.Header.Set(key, processedValue)
	}

	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start).Milliseconds()
		lte.safeChannelSend(session, result)
		return
	}
	defer resp.Body.Close()

	responseBody, _ := io.ReadAll(resp.Body)
	result.ResponseBody = string(responseBody)
	result.StatusCode = resp.StatusCode
	result.Duration = time.Since(start).Milliseconds()

	lte.safeChannelSend(session, result)
}
