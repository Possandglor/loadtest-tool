# Новые возможности Load Test Tool

## 1. Sequential Scenarios (Последовательные сценарии)

Выполнение запросов последовательно с извлечением данных из предыдущих шагов.

### Пример конфигурации:

```json
{
  "name": "Login and Get Profile",
  "is_sequential": true,
  "rps": 5,
  "duration": 60,
  "steps": [
    {
      "order": 1,
      "name": "Login",
      "url": "https://api.example.com/login",
      "method": "POST",
      "headers": {"Content-Type": "application/json"},
      "body": "{\"username\":\"test\",\"password\":\"pass\"}",
      "extractors": [
        {
          "name": "user_id",
          "json_path": "$.data.id"
        },
        {
          "name": "token",
          "json_path": "$.token"
        }
      ]
    },
    {
      "order": 2,
      "name": "Get Profile",
      "url": "https://api.example.com/users/{{user_id}}",
      "method": "GET",
      "headers": {
        "Authorization": "Bearer {{token}}"
      }
    }
  ]
}
```

### Extractors (Извлечение данных):

- **json_path**: Путь к значению в JSON ответе (например: `$.data.id`)
- **regex**: Регулярное выражение для извлечения (например: `"token":"([^"]+)"`)
- **header**: Извлечение из заголовка ответа

### Логика работы:

1. Каждый RPS запускает полный сценарий
2. Шаги выполняются последовательно
3. Переменные из extractors доступны в следующих шагах через `{{variable_name}}`
4. При ошибке на любом шаге сценарий прерывается

## 2. Random Weighted Requests (Случайные запросы с весами)

Случайный выбор запросов с учетом весов для имитации реальной нагрузки.

### Пример конфигурации:

```json
{
  "name": "Mixed API Load",
  "is_random": true,
  "rps": 20,
  "duration": 120,
  "weighted_requests": [
    {
      "name": "Read Operation",
      "url": "https://api.example.com/items",
      "method": "GET",
      "weight": 70
    },
    {
      "name": "Write Operation",
      "url": "https://api.example.com/items",
      "method": "POST",
      "body": "{\"name\":\"{{randomString:10}}\"}",
      "weight": 20
    },
    {
      "name": "Delete Operation",
      "url": "https://api.example.com/items/{{randomInt:1,100}}",
      "method": "DELETE",
      "weight": 10
    }
  ]
}
```

### Weight (Вес):

- Определяет вероятность выбора запроса
- В примере выше: 70% GET, 20% POST, 10% DELETE
- Сумма весов может быть любой (автоматически нормализуется)

### Логика работы:

1. Каждый RPS выбирает случайный запрос согласно весам
2. Все динамические функции работают (`{{randomString}}`, `{{uuid}}`, и т.д.)
3. Переменные сессии доступны через `{{variable_name}}`

## Использование через API

### Sequential Scenario:

```bash
curl -X POST http://localhost:8080/api/configs \
  -H "Content-Type: application/json" \
  -d @examples/sequential_scenario.json
```

### Random Weighted:

```bash
curl -X POST http://localhost:8080/api/configs \
  -H "Content-Type: application/json" \
  -d @examples/random_weighted.json
```

## Совместимость

Все существующие конфигурации продолжают работать:
- Обычные тесты (без `is_sequential` и `is_random`)
- Динамические RPS шаги
- Token authentication
- Body variants

## Приоритет режимов:

1. `is_sequential: true` → Sequential Scenario
2. `is_random: true` → Random Weighted
3. `rps_steps` → Dynamic RPS
4. По умолчанию → Parallel requests
