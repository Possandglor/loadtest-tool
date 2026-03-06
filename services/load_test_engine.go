package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"loadtest-tool/models"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type LoadTestEngine struct {
	tokenService      *TokenService
	resultsChan       chan models.TestResult
	metricsChan       chan models.MetricsSnapshot
	sessions          map[string]*TestSession
	mutex             sync.RWMutex
	onSessionComplete func(sessionID string) // callback при завершении сессии
}

type TestSession struct {
	ID          string
	Config      *models.TestConfig
	Cancel      context.CancelFunc
	IsActive    bool
	Variables   map[string]string
	VarMutex    sync.RWMutex
	ResultsChan chan models.TestResult
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
	
	// Вызываем callback при остановке
	if lte.onSessionComplete != nil {
		go lte.onSessionComplete(sessionID)
	}
	
	return nil
}

func (lte *LoadTestEngine) runTest(ctx context.Context, session *TestSession) {
	defer func() {
		lte.mutex.Lock()
		session.IsActive = false
		lte.mutex.Unlock()
		
		// Вызываем callback при завершении
		if lte.onSessionComplete != nil {
			lte.onSessionComplete(session.ID)
		}
	}()

	config := session.Config
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// Если есть RPS шаги, используем их, иначе базовый RPS
	if len(config.RPSSteps) > 0 {
		lte.runDynamicRPSTest(ctx, session, client)
	} else {
		lte.runStaticRPSTest(ctx, session, client, config.RPS)
	}
}

func (lte *LoadTestEngine) runDynamicRPSTest(ctx context.Context, session *TestSession, client *http.Client) {
	for _, step := range session.Config.RPSSteps {
		stepCtx, stepCancel := context.WithTimeout(ctx, time.Duration(step.Duration)*time.Second)
		
		// Запускаем тест с текущим RPS
		go lte.runStaticRPSTest(stepCtx, session, client, step.RPS)
		
		// Ждем завершения шага или отмены
		select {
		case <-stepCtx.Done():
			stepCancel()
		case <-ctx.Done():
			stepCancel()
			return
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
	start := time.Now()
	config := session.Config
	result := models.TestResult{
		ID:        uuid.New().String(),
		TestID:    config.ID,
		Timestamp: start,
	}

	// Проверяем что сессия активна перед отправкой
	lte.mutex.RLock()
	isActive := session.IsActive
	lte.mutex.RUnlock()
	
	if !isActive {
		return
	}

	// Подготавливаем запрос
	var body io.Reader
	var bodyText string
	
	// Выбираем body: если есть варианты - случайный, иначе основной
	if len(config.BodyVariants) > 0 {
		bodyText = config.BodyVariants[rand.Intn(len(config.BodyVariants))]
	} else if config.Body != "" {
		bodyText = config.Body
	}
	
	if bodyText != "" {
		processedBody := lte.replaceVariables(bodyText, session)
		body = bytes.NewBufferString(processedBody)
		result.RequestBody = processedBody // Сохраняем request body
	}

	processedURL := lte.replaceVariables(config.URL, session)
	req, err := http.NewRequest(config.Method, processedURL, body)
	if err != nil {
		result.Error = err.Error()
		lte.safeChannelSend(session, result)
		return
	}

	// Устанавливаем заголовки с заменой переменных
	for key, value := range config.Headers {
		processedValue := lte.replaceVariables(value, session)
		req.Header.Set(key, processedValue)
	}

	// Выполняем запрос
	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start).Milliseconds()
		result.StatusCode = 0
		lte.safeChannelSend(session, result)
		return
	}
	defer resp.Body.Close()

	// Читаем ответ
	responseBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		result.Error = readErr.Error()
	} else {
		result.ResponseBody = string(responseBody) // Сохраняем response body
	}

	result.StatusCode = resp.StatusCode
	result.Duration = time.Since(start).Milliseconds()
	lte.safeChannelSend(session, result)
}

func (lte *LoadTestEngine) safeChannelSend(session *TestSession, result models.TestResult) {
	defer func() {
		if r := recover(); r != nil {
			// Канал закрыт, игнорируем
		}
	}()
	
	select {
	case session.ResultsChan <- result:
	default:
		// Канал заполнен, пропускаем
	}
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
	
	lte.mutex.RLock()
	session, exists := lte.sessions[sessionID]
	lte.mutex.RUnlock()
	
	if !exists {
		return
	}
	
	defer func() {
		// Безопасно закрываем канал
		defer func() {
			if r := recover(); r != nil {
				// Канал уже закрыт
			}
		}()
		close(session.ResultsChan)
	}()
	
	for {
		select {
		case <-ticker.C:
			if len(results) > 0 {
				snapshot := lte.calculateMetrics(results)
				lte.metricsChan <- snapshot
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
				return // Канал закрыт
			}
			results = append(results, result)
			
			// Пересылаем результат в главный канал для сохранения в БД
			select {
			case lte.resultsChan <- result:
			default:
				// Канал заполнен, пропускаем
			}
		}
	}
}

func (lte *LoadTestEngine) calculateMetrics(results []models.TestResult) models.MetricsSnapshot {
	snapshot := models.MetricsSnapshot{
		Timestamp:   time.Now(),
		RPS:         len(results),
		StatusCodes: make(map[int]int),
	}

	if len(results) == 0 {
		return snapshot
	}

	var totalDuration int64
	var errorCount int
	var lastError string
	durations := make([]int64, 0, len(results))

	for _, result := range results {
		totalDuration += result.Duration
		durations = append(durations, result.Duration)
		snapshot.StatusCodes[result.StatusCode]++
		
		if result.Error != "" || result.StatusCode >= 400 {
			errorCount++
			if result.Error != "" {
				lastError = result.Error
			}
		}
	}

	snapshot.AvgDuration = float64(totalDuration) / float64(len(results))
	snapshot.ErrorRate = float64(errorCount) / float64(len(results)) * 100
	snapshot.LastError = lastError

	// Вычисляем перцентили
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

func (lte *LoadTestEngine) SetOnSessionComplete(callback func(sessionID string)) {
	lte.onSessionComplete = callback
}
