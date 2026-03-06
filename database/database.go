package database

import (
	"database/sql"
	"encoding/json"
	"loadtest-tool/models"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	conn *sql.DB
}

func NewDB(dbPath string) (*DB, error) {
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		return nil, err
	}

	return db, nil
}

func (db *DB) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS test_configs (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			method TEXT NOT NULL,
			headers TEXT,
			body TEXT,
			rps INTEGER NOT NULL,
			duration INTEGER NOT NULL,
			token_config TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS test_sessions (
			id TEXT PRIMARY KEY,
			test_id TEXT NOT NULL,
			status TEXT NOT NULL,
			started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			ended_at DATETIME,
			FOREIGN KEY (test_id) REFERENCES test_configs (id)
		)`,
		`CREATE TABLE IF NOT EXISTS test_results (
			id TEXT PRIMARY KEY,
			test_id TEXT NOT NULL,
			status_code INTEGER NOT NULL,
			duration INTEGER NOT NULL,
			error TEXT,
			request_body TEXT,
			response_body TEXT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (test_id) REFERENCES test_configs (id)
		)`,
		// Добавляем колонку rps_steps если её нет
		`ALTER TABLE test_configs ADD COLUMN rps_steps TEXT`,
		// Добавляем колонку body_variants если её нет
		`ALTER TABLE test_configs ADD COLUMN body_variants TEXT`,
		// Добавляем колонки для request/response body
		`ALTER TABLE test_results ADD COLUMN request_body TEXT`,
		`ALTER TABLE test_results ADD COLUMN response_body TEXT`,
	}

	for _, query := range queries {
		if _, err := db.conn.Exec(query); err != nil {
			// Игнорируем ошибку если колонка уже существует
			if !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
		}
	}

	return nil
}

func (db *DB) SaveTestConfig(config *models.TestConfig) error {
	headers, _ := json.Marshal(config.Headers)
	tokenConfig, _ := json.Marshal(config.TokenConfig)
	rpsSteps, _ := json.Marshal(config.RPSSteps)
	bodyVariants, _ := json.Marshal(config.BodyVariants)

	_, err := db.conn.Exec(`
		INSERT OR REPLACE INTO test_configs 
		(id, name, url, method, headers, body, body_variants, rps, duration, rps_steps, token_config, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		config.ID, config.Name, config.URL, config.Method,
		string(headers), config.Body, string(bodyVariants), config.RPS, config.Duration,
		string(rpsSteps), string(tokenConfig), config.CreatedAt)

	return err
}

func (db *DB) GetTestConfigs() ([]models.TestConfig, error) {
	rows, err := db.conn.Query(`
		SELECT id, name, url, method, headers, body, 
		       COALESCE(body_variants, '') as body_variants,
		       rps, duration, 
		       COALESCE(rps_steps, '') as rps_steps,
		       COALESCE(token_config, '') as token_config, 
		       created_at 
		FROM test_configs ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []models.TestConfig
	for rows.Next() {
		var config models.TestConfig
		var headersJSON, bodyVariantsJSON, tokenConfigJSON, rpsStepsJSON string

		err := rows.Scan(&config.ID, &config.Name, &config.URL, &config.Method,
			&headersJSON, &config.Body, &bodyVariantsJSON, &config.RPS, &config.Duration,
			&rpsStepsJSON, &tokenConfigJSON, &config.CreatedAt)
		if err != nil {
			continue
		}

		json.Unmarshal([]byte(headersJSON), &config.Headers)
		if bodyVariantsJSON != "" {
			json.Unmarshal([]byte(bodyVariantsJSON), &config.BodyVariants)
		}
		if tokenConfigJSON != "" {
			json.Unmarshal([]byte(tokenConfigJSON), &config.TokenConfig)
		}
		if rpsStepsJSON != "" {
			json.Unmarshal([]byte(rpsStepsJSON), &config.RPSSteps)
		}

		configs = append(configs, config)
	}

	return configs, nil
}

func (db *DB) SaveTestResult(result *models.TestResult) error {
	_, err := db.conn.Exec(`
		INSERT INTO test_results (id, test_id, status_code, duration, error, request_body, response_body, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		result.ID, result.TestID, result.StatusCode, result.Duration,
		result.Error, result.RequestBody, result.ResponseBody, result.Timestamp)

	return err
}

func (db *DB) GetTestResults(testID string, since time.Time) ([]models.TestResult, error) {
	rows, err := db.conn.Query(`
		SELECT id, test_id, status_code, duration, error, 
		       COALESCE(request_body, '') as request_body,
		       COALESCE(response_body, '') as response_body,
		       timestamp
		FROM test_results 
		WHERE test_id = ? AND timestamp >= ? 
		ORDER BY timestamp DESC`,
		testID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []models.TestResult
	for rows.Next() {
		var result models.TestResult
		err := rows.Scan(&result.ID, &result.TestID, &result.StatusCode,
			&result.Duration, &result.Error, &result.RequestBody, 
			&result.ResponseBody, &result.Timestamp)
		if err != nil {
			continue
		}
		results = append(results, result)
	}

	return results, nil
}

func (db *DB) GetTestConfig(id string) (*models.TestConfig, error) {
	row := db.conn.QueryRow(`
		SELECT id, name, url, method, headers, body, rps, duration, 
		       COALESCE(rps_steps, '') as rps_steps,
		       COALESCE(token_config, '') as token_config, 
		       created_at 
		FROM test_configs WHERE id = ?`, id)
	
	var config models.TestConfig
	var headersJSON, tokenConfigJSON, rpsStepsJSON string

	err := row.Scan(&config.ID, &config.Name, &config.URL, &config.Method,
		&headersJSON, &config.Body, &config.RPS, &config.Duration,
		&rpsStepsJSON, &tokenConfigJSON, &config.CreatedAt)
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(headersJSON), &config.Headers)
	if tokenConfigJSON != "" {
		json.Unmarshal([]byte(tokenConfigJSON), &config.TokenConfig)
	}
	if rpsStepsJSON != "" {
		json.Unmarshal([]byte(rpsStepsJSON), &config.RPSSteps)
	}

	return &config, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) SaveTestSession(sessionID, testID string) error {
	_, err := db.conn.Exec(`
		INSERT INTO test_sessions (id, test_id, status, started_at)
		VALUES (?, ?, ?, ?)`,
		sessionID, testID, "running", time.Now())
	return err
}

func (db *DB) UpdateTestSession(sessionID string, status string, endedAt time.Time) error {
	_, err := db.conn.Exec(`
		UPDATE test_sessions SET status = ?, ended_at = ? WHERE id = ?`,
		status, endedAt, sessionID)
	return err
}

func (db *DB) GetTestSessions(testID string) ([]map[string]interface{}, error) {
	rows, err := db.conn.Query(`
		SELECT id, test_id, status, started_at, ended_at 
		FROM test_sessions 
		WHERE test_id = ? 
		ORDER BY started_at DESC`,
		testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []map[string]interface{}
	for rows.Next() {
		var id, testID, status string
		var startedAt, endedAt sql.NullTime

		err := rows.Scan(&id, &testID, &status, &startedAt, &endedAt)
		if err != nil {
			continue
		}

		session := map[string]interface{}{
			"id":         id,
			"test_id":    testID,
			"status":     status,
			"started_at": startedAt.Time,
		}
		if endedAt.Valid {
			session["ended_at"] = endedAt.Time
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

func (db *DB) GetActiveSessions() ([]map[string]interface{}, error) {
	rows, err := db.conn.Query(`
		SELECT s.id, s.test_id, s.status, s.started_at, c.name
		FROM test_sessions s
		JOIN test_configs c ON s.test_id = c.id
		WHERE s.status = 'running'
		ORDER BY s.started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []map[string]interface{}
	for rows.Next() {
		var id, testID, status, testName string
		var startedAt sql.NullTime

		err := rows.Scan(&id, &testID, &status, &startedAt, &testName)
		if err != nil {
			continue
		}

		sessions = append(sessions, map[string]interface{}{
			"id":         id,
			"test_id":    testID,
			"test_name":  testName,
			"status":     status,
			"started_at": startedAt.Time,
		})
	}

	return sessions, nil
}

func (db *DB) GetTestSession(sessionID string) (map[string]interface{}, error) {
	row := db.conn.QueryRow(`
		SELECT id, test_id, status, started_at, ended_at 
		FROM test_sessions 
		WHERE id = ?`, sessionID)

	var id, testID, status string
	var startedAt, endedAt sql.NullTime

	err := row.Scan(&id, &testID, &status, &startedAt, &endedAt)
	if err != nil {
		return nil, err
	}

	session := map[string]interface{}{
		"id":         id,
		"test_id":    testID,
		"status":     status,
		"started_at": startedAt.Time,
	}
	if endedAt.Valid {
		session["ended_at"] = endedAt.Time
	}

	return session, nil
}
