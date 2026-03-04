BINARY_NAME=loadtest-tool
BUILD_DIR=build

.PHONY: build run clean deps test

# Сборка приложения
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) .

# Запуск приложения
run: build
	@echo "Starting $(BINARY_NAME)..."
	./$(BUILD_DIR)/$(BINARY_NAME)

# Установка зависимостей
deps:
	@echo "Installing dependencies..."
	go mod tidy
	go mod download

# Очистка
clean:
	@echo "Cleaning..."
	rm -rf $(BUILD_DIR)
	rm -f loadtest.db

# Тестирование
test:
	go test -v ./...

# Запуск в dev режиме
dev:
	go run .

# Сборка для разных платформ
build-all:
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 .
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe .
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 .
