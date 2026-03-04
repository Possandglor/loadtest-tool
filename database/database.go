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
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (test_id) REFERENCES test_configs (id)
		)`,
		// Добавляем колонку rps_steps если её нет
		`ALTER TABLE test_configs ADD COLUMN rps_steps TEXT`,
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

	_, err := db.conn.Exec(`
		INSERT OR REPLACE INTO test_configs 
		(id, name, url, method, headers, body, rps, duration, rps_steps, token_config, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		config.ID, config.Name, config.URL, config.Method,
		string(headers), config.Body, config.RPS, config.Duration,
		string(rpsSteps), string(tokenConfig), config.CreatedAt)

	return err
}

func (db *DB) GetTestConfigs() ([]models.TestConfig, error) {
	rows, err := db.conn.Query(`
		SELECT id, name, url, method, headers, body, rps, duration, 
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
		var headersJSON, tokenConfigJSON, rpsStepsJSON string

		err := rows.Scan(&config.ID, &config.Name, &config.URL, &config.Method,
			&headersJSON, &config.Body, &config.RPS, &config.Duration,
			&rpsStepsJSON, &tokenConfigJSON, &config.CreatedAt)
		if err != nil {
			continue
		}

		json.Unmarshal([]byte(headersJSON), &config.Headers)
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
		INSERT INTO test_results (id, test_id, status_code, duration, error, timestamp)
		VALUES (?, ?, ?, ?, ?, ?)`,
		result.ID, result.TestID, result.StatusCode, result.Duration,
		result.Error, result.Timestamp)

	return err
}

func (db *DB) GetTestResults(testID string, since time.Time) ([]models.TestResult, error) {
	rows, err := db.conn.Query(`
		SELECT * FROM test_results 
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
			&result.Duration, &result.Error, &result.Timestamp)
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
