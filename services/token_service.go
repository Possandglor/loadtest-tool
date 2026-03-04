package services

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"loadtest-tool/models"
	"net/http"
	"strings"
	"sync"
	"time"
)

type TokenCache struct {
	token     string
	expiresAt time.Time
}

type TokenService struct {
	cache map[string]*TokenCache
	mutex sync.RWMutex
}

func NewTokenService() *TokenService {
	return &TokenService{
		cache: make(map[string]*TokenCache),
	}
}

func (ts *TokenService) GetToken(config *models.TokenConfig) (string, error) {
	if config == nil {
		return "", nil
	}

	cacheKey := fmt.Sprintf("%s:%s", config.URL, config.TokenPath)

	ts.mutex.RLock()
	cached, exists := ts.cache[cacheKey]
	ts.mutex.RUnlock()

	if exists && time.Now().Before(cached.expiresAt) {
		return cached.token, nil
	}

	token, err := ts.fetchToken(config)
	if err != nil {
		return "", err
	}

	ts.mutex.Lock()
	ts.cache[cacheKey] = &TokenCache{
		token:     token,
		expiresAt: time.Now().Add(time.Duration(config.CacheTTL) * time.Second),
	}
	ts.mutex.Unlock()

	return token, nil
}

func (ts *TokenService) fetchToken(config *models.TokenConfig) (string, error) {
	var body io.Reader
	if config.Body != "" {
		body = bytes.NewBufferString(config.Body)
	}

	req, err := http.NewRequest(config.Method, config.URL, body)
	if err != nil {
		return "", err
	}

	for key, value := range config.Headers {
		req.Header.Set(key, value)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var jsonResp map[string]interface{}
	if err := json.Unmarshal(respBody, &jsonResp); err != nil {
		return "", err
	}

	token := ts.extractTokenFromJSON(jsonResp, config.TokenPath)
	if token == "" {
		return "", fmt.Errorf("token not found at path: %s", config.TokenPath)
	}

	return token, nil
}

func (ts *TokenService) extractTokenFromJSON(data map[string]interface{}, path string) string {
	parts := strings.Split(path, ".")
	current := data

	for i, part := range parts {
		if i == len(parts)-1 {
			if val, ok := current[part].(string); ok {
				return val
			}
			return ""
		}

		if next, ok := current[part].(map[string]interface{}); ok {
			current = next
		} else {
			return ""
		}
	}

	return ""
}
