# План разработки торгового ядра

## Обозначения статусов

```
[ ] - Задача не выполнена (ожидает выполнения)
[x] - Задача выполнена
```

---

## Общая информация

**Цель:** Создать высокопроизводительное торговое ядро для автоматического арбитража фьючерсов между 6 криптовалютными биржами.

**Ключевые метрики:**
- Латентность: Tick → Order < 5ms
- Мониторинг: до 30 пар на 6 биржах (180 потоков цен)
- Надёжность: работа 24/7 с автоматическим восстановлением

**Поддерживаемые биржи:** Bybit, Bitget, BingX, Gate.io, OKX, HTX, MEXC

---

## Этап 1: Инфраструктура и базовая настройка

### 1.1. Инициализация проекта
- [x] Создать структуру проекта согласно архитектуре
- [x] Инициализировать Go модуль (`go.mod`)
- [x] Добавить основные зависимости:
  - `github.com/gorilla/websocket` или `nhooyr.io/websocket`
  - `github.com/json-iterator/go` (jsoniter)
  - `github.com/lib/pq` (PostgreSQL driver)
  - `github.com/prometheus/client_golang`
  - `gopkg.in/yaml.v3` (для конфигурации)
  - Logging библиотека (например, `go.uber.org/zap`)

### 1.2. Конфигурация
- [x] Создать `internal/config/config.go` - загрузка конфигурации из YAML
- [x] Создать `internal/config/fees.go` - захардкодить комиссии бирж (taker fees)
- [x] Создать `configs/app.yaml` - основной конфигурационный файл (шаблон)
- [x] Создать `configs/exchanges.json` - минимумы и лимиты бирж (шаблон)
- [x] Реализовать загрузку переменных окружения для API ключей

### 1.3. База данных
- [x] Создать `internal/db/models.go` - модели данных (PairConfig, Trade, LeverageCache)
- [x] Создать SQL миграции:
  - `internal/db/migrations/001_create_pairs.up.sql`
  - `internal/db/migrations/001_create_pairs.down.sql`
  - `internal/db/migrations/002_create_trades.up.sql`
  - `internal/db/migrations/002_create_trades.down.sql`
  - `internal/db/migrations/003_create_leverage_cache.up.sql`
  - `internal/db/migrations/003_create_leverage_cache.down.sql`
- [x] Создать `internal/db/repository.go` - CRUD операции для работы с БД
- [x] Добавить инструмент для миграций (например, `golang-migrate/migrate`)

---

## Этап 2: Общие интерфейсы и типы данных

### 2.1. Общие типы для бирж
- [ ] Создать `internal/exchanges/common.go` - общий интерфейс Exchange:
  - `PlaceOrder(req *OrderRequest) (*OrderResponse, error)`
  - `GetBalance(asset string) (*Balance, error)`
  - `SetLeverage(symbol string, leverage int) error`
  - `Connect() error`
  - `Close() error`
- [ ] Определить типы:
  - `OrderRequest` (Symbol, Side, Type, Quantity, PositionSide)
  - `OrderResponse` (OrderID, FilledQty, AvgPrice, Status)
  - `Balance` (Asset, Available, InPosition)

### 2.2. Типы для цен и стаканов
- [ ] Создать `internal/core/orderbook/orderbook.go`:
  - Тип `OrderBook` с уровнями (Level: Price, Quantity)
  - Метод `CalculateAvgPrice(volume float64, side string) float64`
- [ ] Создать `internal/core/prices/tracker.go`:
  - Тип `Tracker` для отслеживания лучших цен по символу
  - Методы `Update()`, `GetBestPrices()`, `GetPrice(exchange, side)`
- [ ] Создать `internal/core/prices/aggregator.go`:
  - Тип `Aggregator` для сбора цен со всех бирж
  - Методы `Subscribe(symbol)`, `handlePriceUpdate()`

### 2.3. State Machine
- [ ] Создать `internal/state/machine.go`:
  - Определить enum состояний: PAUSED, READY, ENTERING, POSITION_OPEN, EXITING, ERROR
  - Тип `Machine` с методами `Transition()`, `CurrentState()`, `CanTransition()`
- [ ] Создать `internal/state/transitions.go`:
  - Определить разрешённые переходы между состояниями (map)

---

## Этап 3: Переиспользуемые компоненты (pkg/)

### 3.1. Rate Limiting
- [ ] Создать `pkg/ratelimit/limiter.go`:
  - Реализация Token Bucket алгоритма
  - Методы `Wait()`, `TryAcquire()`

### 3.2. Object Pooling
- [ ] Создать `pkg/pool/object_pool.go`:
  - Пулы для переиспользования объектов:
    - `PriceUpdatePool`
    - `BestPricesPool`

### 3.3. Метрики Prometheus
- [ ] Создать `pkg/metrics/prometheus.go`:
  - Определить метрики:
    - `tick_to_order_duration_ms` (Histogram)
    - `spread_calculation_duration_ms` (Histogram)
    - `order_execution_duration_ms` (Histogram)
    - `websocket_events_total` (Counter)
    - `orders_total` (Counter)
    - `arbitrage_cycles_total` (Counter)
    - `buffer_overflow_total` (Counter)
  - Метод регистрации метрик

---

## Этап 4: Коннекторы бирж (по одному за раз)

**ВАЖНО:** Разрабатывать коннекторы ПОСЛЕДОВАТЕЛЬНО. Начать с одной биржи (Bybit), протестировать, затем переходить к следующей. Это минимизирует ошибки и переписывание кода.

### 4.1. Bybit коннектор (первый, самый детальный)
- [ ] Создать `internal/exchanges/bybit/types.go`:
  - Специфичные типы для Bybit API
  - `BybitOrderResponse`, `BybitWebSocketMessage`
- [ ] Создать `internal/exchanges/bybit/rest.go`:
  - Методы REST API:
    - `placeOrderREST()`
    - `getBalanceREST()`
    - `setLeverageREST()`
    - `getInstrumentInfoREST()` (для минимумов)
  - Метод `doRequest()` с подписью запросов
  - Rate limiter (50% от лимита: 60 req/min)
- [ ] Создать `internal/exchanges/bybit/parser.go`:
  - Парсинг JSON ответов Bybit
  - `parseOrderbookMessage()`
  - `parseOrderResponse()`
- [ ] Создать `internal/exchanges/bybit/websocket.go`:
  - WebSocket клиент с автоматическим reconnect
  - Типы: `WebSocketClient`, поля: conn, url, subscriptions, callback, reconnectCh, closeCh
  - Методы:
    - `Connect()` - подключение
    - `handleMessages()` - обработка входящих сообщений
    - `reconnect()` - переподключение с exponential backoff
    - `resubscribe()` - восстановление подписок
    - `sendPing()` - ping/pong для поддержания соединения
  - Callback для отправки PriceUpdate в aggregator
- [ ] Создать `internal/exchanges/bybit/bybit.go`:
  - Главный клиент `Client` (реализация интерфейса Exchange)
  - Инициализация REST и WebSocket клиентов
  - Реализация всех методов интерфейса
- [ ] **Протестировать Bybit коннектор:**
  - Подключение к WebSocket
  - Получение данных стакана
  - Выставление тестового ордера (минимальный объём)
  - Проверка баланса
  - Установка плеча
  - Reconnect логика

### 4.2. Bitget коннектор
- [ ] Создать `internal/exchanges/bitget/types.go`
- [ ] Создать `internal/exchanges/bitget/rest.go`
- [ ] Создать `internal/exchanges/bitget/parser.go`
- [ ] Создать `internal/exchanges/bitget/websocket.go`
- [ ] Создать `internal/exchanges/bitget/bitget.go`
- [ ] **Протестировать Bitget коннектор** (аналогично Bybit)

### 4.3. BingX коннектор
- [ ] Создать `internal/exchanges/bingx/types.go`
- [ ] Создать `internal/exchanges/bingx/rest.go`
- [ ] Создать `internal/exchanges/bingx/parser.go`
- [ ] Создать `internal/exchanges/bingx/websocket.go`
- [ ] Создать `internal/exchanges/bingx/bingx.go`
- [ ] **Протестировать BingX коннектор**

### 4.4. Gate.io коннектор
- [ ] Создать `internal/exchanges/gate/types.go`
- [ ] Создать `internal/exchanges/gate/rest.go`
- [ ] Создать `internal/exchanges/gate/parser.go`
- [ ] Создать `internal/exchanges/gate/websocket.go`
- [ ] Создать `internal/exchanges/gate/gate.go`
- [ ] **Протестировать Gate.io коннектор**

### 4.5. OKX коннектор
- [ ] Создать `internal/exchanges/okx/types.go`
- [ ] Создать `internal/exchanges/okx/rest.go`
- [ ] Создать `internal/exchanges/okx/parser.go`
- [ ] Создать `internal/exchanges/okx/websocket.go`
- [ ] Создать `internal/exchanges/okx/okx.go`
- [ ] **Протестировать OKX коннектор**

### 4.6. HTX коннектор
- [ ] Создать `internal/exchanges/htx/types.go`
- [ ] Создать `internal/exchanges/htx/rest.go`
- [ ] Создать `internal/exchanges/htx/parser.go`
- [ ] Создать `internal/exchanges/htx/websocket.go`
- [ ] Создать `internal/exchanges/htx/htx.go`
- [ ] **Протестировать HTX коннектор**

### 4.7. MEXC коннектор
- [ ] Создать `internal/exchanges/mexc/types.go`
- [ ] Создать `internal/exchanges/mexc/rest.go`
- [ ] Создать `internal/exchanges/mexc/parser.go`
- [ ] Создать `internal/exchanges/mexc/websocket.go`
- [ ] Создать `internal/exchanges/mexc/mexc.go`
- [ ] **Протестировать MEXC коннектор**

---

## Этап 5: Ядро торговой логики

**ВАЖНО:** Разрабатывать модули арбитража ПОСЛЕДОВАТЕЛЬНО, снизу вверх (от простых к сложным).

### 5.1. Управление рисками
- [ ] Создать `internal/risk/limits.go`:
  - `ValidateVolume(symbol, exchange, volume)` - проверка минимальных объёмов
  - `GetMinOrderSize(exchange, symbol)` - получение минимума
- [ ] Создать `internal/risk/balance.go`:
  - `CheckBalance(exchange, requiredMargin)` - проверка баланса
  - `CalculateRequiredMargin(volume, price, leverage)` - расчёт маржи
- [ ] Создать `internal/risk/stoploss.go`:
  - `CheckStopLoss(currentPNL, stopLossLimit)` - проверка Stop Loss
  - `CalculatePNL(position, currentPrices)` - расчёт PNL

### 5.2. Логика арбитража для одной пары
- [ ] Создать `internal/core/arbitrage/pair.go`:
  - Тип `Pair` с полями:
    - id, config, state, position, priceTracker, riskManager, exchanges, balanceCache
  - Тип `Position` с полями:
    - ExchangeLong, ExchangeShort, EntryPriceLong, EntryPriceShort, Volume, FilledParts, TotalParts, UnrealizedPNL, OpenedAt
  - Метод `OnPriceUpdate(update)` - главный обработчик событий цен:
    - Switch по текущему состоянию (READY, POSITION_OPEN)
    - Проверка условий входа/выхода
  - Метод `checkEntryConditions()` - проверка условий входа:
    - Спред >= порога
    - Свободный слот для арбитража
    - Достаточный баланс
  - Метод `checkExitConditions()` - проверка условий выхода:
    - Спред <= порога выхода
    - Stop Loss не сработал
  - Методы вспомогательные:
    - `updateUnrealizedPNL()`
    - `selectExchanges()` - выбор бирж (дешёвая/дорогая)

### 5.3. Логика входа в позицию
- [ ] Создать `internal/core/arbitrage/entry.go`:
  - Метод `enterPosition()` - главный метод входа:
    - Установка плеча (если нужно)
    - Проверка спреда
    - Вход частями (цикл по NumOrders)
    - Переход в POSITION_OPEN после полного входа
  - Метод `enterPart(partIndex)` - вход одной частью:
    - Расчёт объёма части
    - Выбор бирж
    - Параллельное выставление ордеров
  - Метод `executeOrdersPair(longExch, shortExch, volume)`:
    - Параллельное выставление long и short ордеров
    - Обработка ошибок (если одна нога не открылась)
  - Метод `retrySecondLeg()` - retry второй ноги (до 3 попыток с backoff)
  - Метод `rollbackFirstLeg()` - закрытие первой ноги при ошибке второй
  - Метод `rollbackSecondLeg()` - закрытие второй ноги при ошибке первой
  - Метод `setLeverageIfNeeded()` - установка плеча с кэшированием:
    - Проверка кэша в БД
    - Установка через REST API
    - Сохранение в кэш
  - Метод `updatePositionAfterEntry()` - обновление позиции после успешного входа
  - Метод `handleEntryError()` - обработка ошибок входа

### 5.4. Логика выхода из позицию
- [ ] Создать `internal/core/arbitrage/exit.go`:
  - Метод `exitPosition()` - главный метод выхода:
    - Выход частями (зеркально входу)
    - Проверка спреда перед каждой частью
    - **Плавающее состояние:** если спред расширился обратно, можем добрать позицию
    - Финализация позиции после полного выхода
  - Метод `exitPart(partIndex)` - выход одной частью:
    - Расчёт объёма части
    - Параллельное закрытие long и short позиций
  - Метод `finalizePosition()`:
    - Расчёт реализованного PNL
    - Сохранение сделки в БД (таблица trades)
    - Обнуление позиции
    - Переход в READY
  - Метод `calculateRealizedPNL()` - расчёт прибыли/убытка

### 5.5. Мониторинг открытой позиции
- [ ] Создать `internal/core/arbitrage/monitor.go`:
  - Метод `monitorPosition()` - фоновый мониторинг:
    - Ticker каждую секунду
    - Обновление Unrealized PNL
    - Проверка Stop Loss
    - Проверка условий выхода
  - Метод `updateUnrealizedPNL(prices)` - обновление нереализованного PNL:
    - PNL long позиции
    - PNL short позиции
  - Метод `checkStopLoss()` - проверка срабатывания Stop Loss

---

## Этап 6: Координатор и движок

### 6.1. Шардирование событий
- [ ] Создать `internal/core/engine/coordinator.go`:
  - Тип `Coordinator` с полями:
    - numShards, shards, router
  - Тип `Shard` с полями:
    - id, eventChan, pairs (map symbol -> Pair), workers
  - Тип `EventRouter` с полями:
    - shardMap (symbol -> shard_id)
  - Метод `NewCoordinator(numShards, workersPerShard)`:
    - Создание N шардов
    - Запуск workers для каждого шарда
  - Метод `RouteEvent(event)` - маршрутизация события к шарду:
    - Hash-based routing: `shardID = hash(symbol) % numShards`
  - Метод `processEvents()` в Shard - обработка событий worker'ом:
    - Чтение из eventChan
    - Вызов `pair.OnPriceUpdate(event)`

### 6.2. Главный движок
- [ ] Создать `internal/core/engine/engine.go`:
  - Тип `TradingEngine` с полями:
    - coordinator, aggregator, exchanges, db, config, slots (ArbitrageSlots)
  - Тип `ArbitrageSlots`:
    - MaxSlots, ActiveSlots (map), mu
    - Методы: `TryAcquire(symbol)`, `Release(symbol)`, `CanStart()`
  - Метод `NewTradingEngine(config, db)` - инициализация:
    - Создание coordinator
    - Создание aggregator
    - Инициализация коннекторов бирж
    - Загрузка пар из БД
  - Метод `AddPair(config)` - добавление новой пары:
    - Валидация параметров
    - Создание объекта Pair
    - Добавление в соответствующий шард
    - Подписка на символ через aggregator
  - Метод `RemovePair(id)` - удаление пары
  - Метод `Start()` - запуск движка:
    - Подключение к биржам (WebSocket)
    - Запуск coordinator
    - Запуск aggregator
  - Метод `Stop()` - graceful shutdown:
    - Остановка приёма новых событий
    - Закрытие открытых позиций (если есть)
    - Отключение от бирж
  - Метод `RecoverPositions()` - восстановление при рестарте:
    - Получение открытых позиций через REST API
    - Закрытие всех позиций
    - Перевод пар в ПАУЗУ

---

## Этап 7: Точка входа и инициализация

### 7.1. Main функция
- [ ] Создать `cmd/trading-engine/main.go`:
  - Функция `main()`:
    - Загрузка конфигурации (`initConfig()`)
    - Инициализация БД (`initDB()`)
    - Запуск миграций
    - Инициализация бирж (`initExchanges()`)
    - Загрузка минимумов бирж (exchanges.json или через API)
    - Создание TradingEngine
    - Восстановление позиций при рестарте
    - Запуск движка
    - Graceful shutdown (обработка SIGINT, SIGTERM)
  - Функция `initConfig()` - загрузка конфигурации
  - Функция `initDB()` - подключение к PostgreSQL
  - Функция `initExchanges()` - создание коннекторов бирж

### 7.2. Graceful Shutdown
- [ ] Реализовать корректное завершение:
  - Перехват сигналов (os.Signal)
  - Остановка приёма новых событий
  - Закрытие всех открытых позиций
  - Сохранение состояния в БД
  - Закрытие WebSocket соединений
  - Закрытие БД соединения

---

## Этап 8: Интеграционное тестирование

### 8.1. Unit тесты
- [ ] Написать тесты для `internal/core/orderbook` - расчёт средней цены
- [ ] Написать тесты для `internal/core/prices/tracker` - обновление и получение цен
- [ ] Написать тесты для `internal/state/machine` - переходы между состояниями
- [ ] Написать тесты для `internal/risk` - проверки балансов и лимитов
- [ ] Написать тесты для `pkg/ratelimit` - Token Bucket алгоритм

### 8.2. Интеграционные тесты
- [ ] Тест подключения к биржам (все 7 бирж):
  - Успешное WebSocket подключение
  - Получение данных orderbook
  - Reconnect при обрыве
- [ ] Тест выставления ордеров (на testnet если доступен):
  - Открытие long позиции
  - Открытие short позиции
  - Закрытие позиций
- [ ] Тест полного цикла арбитража (на минимальных объёмах):
  - Вход 1 часть
  - Вход N частей
  - Мониторинг позиции
  - Выход частями
  - Проверка сохранения в БД
- [ ] Тест Stop Loss:
  - Эмуляция убыточной позиции
  - Проверка срабатывания SL
  - Закрытие позиций
- [ ] Тест плавающего состояния:
  - Частичный вход (2 из 4 частей)
  - Спред схлопнулся
  - Начало выхода
  - Спред расширился обратно
  - Добор позиции

### 8.3. Стресс-тесты
- [ ] Тест обработки 30 пар на 6 биржах (180 потоков):
  - Запуск мониторинга всех пар
  - Проверка отсутствия потери событий
  - Измерение латентности (Tick → Order)
  - Проверка использования памяти
- [ ] Тест длительной работы (24 часа):
  - Запуск на VPS
  - Мониторинг стабильности
  - Проверка отсутствия утечек памяти
  - Проверка reconnect логики

---

## Этап 9: Оптимизация производительности

### 9.1. Профилирование
- [ ] Запустить pprof для CPU профилирования:
  - Найти горячие точки (hot paths)
  - Оптимизировать узкие места
- [ ] Запустить pprof для памяти:
  - Найти аллокации в hot path
  - Применить object pooling где необходимо
- [ ] Измерить латентность:
  - Tick → Order (цель < 5ms)
  - Расчёт спреда (цель < 0.5ms)
  - Парсинг JSON (цель < 0.1ms)

### 9.2. Оптимизации
- [ ] Применить `jsoniter` вместо стандартного encoding/json
- [ ] Использовать `sync.Pool` для всех часто создаваемых объектов:
  - PriceUpdate
  - BestPrices
  - OrderRequest/Response
- [ ] Применить lock-free чтение где возможно:
  - `sync.Map` для concurrent read/write
  - `atomic` для счётчиков
- [ ] Предаллокация слайсов и буферизованных каналов
- [ ] In-place обновление структур данных где безопасно

---

## Этап 10: Документация и финализация

### 10.1. Документация кода
- [ ] Добавить godoc комментарии ко всем публичным типам и функциям
- [ ] Создать README.md с инструкциями:
  - Требования (Go 1.21+, PostgreSQL 14+)
  - Установка зависимостей
  - Настройка конфигурации
  - Запуск миграций БД
  - Запуск приложения
  - Переменные окружения для API ключей

### 10.2. Логирование и мониторинг
- [ ] Проверить корректность всех логов:
  - DEBUG: каждое WebSocket событие
  - INFO: открытие/закрытие позиций
  - WARN: retry попытки, близость к rate limit
  - ERROR: ошибки API, неудачные ордера
- [ ] Настроить экспорт метрик Prometheus:
  - Endpoint /metrics на порту 9090
  - Проверить все метрики

### 10.3. Конфигурационные файлы
- [ ] Заполнить `configs/app.yaml` реальными параметрами
- [ ] Заполнить `configs/exchanges.json` актуальными минимумами (через API)
- [ ] Создать `.env.example` с примерами переменных окружения

### 10.4. Критерии приёмки (финальная проверка)
- [ ] ✅ Успешно открывает и закрывает арбитраж на 2 биржах (1 полный цикл)
- [ ] ✅ Корректно работает вход/выход частями (N=4)
- [ ] ✅ Stop Loss срабатывает корректно
- [ ] ✅ Reconnect восстанавливает мониторинг после обрыва WebSocket
- [ ] ✅ Латентность Tick → Order < 10ms (измерено на продакшене)
- [ ] ✅ Обрабатывает 30 пар на 6 биржах без потери событий
- [ ] ✅ Работает 24 часа без сбоев на VPS

---

## Приоритеты разработки

**Критический путь (minimum viable product):**
1. Этап 1 (Инфраструктура) → **Обязательно**
2. Этап 2 (Интерфейсы) → **Обязательно**
3. Этап 3 (pkg компоненты) → **Обязательно**
4. Этап 4.1 (Bybit коннектор) → **Обязательно для MVP**
5. Этап 4.2 (Bitget коннектор) → **Обязательно для MVP** (нужны 2 биржи для арбитража)
6. Этап 5 (Ядро логики) → **Обязательно**
7. Этап 6 (Coordinator) → **Обязательно**
8. Этап 7 (Main) → **Обязательно**
9. Этап 8.2 (Интеграционные тесты) → **Обязательно**

**Можно отложить:**
- Этап 4.3-4.7 (остальные биржи) - добавить после MVP
- Этап 8.3 (стресс-тесты) - после основной разработки
- Этап 9 (оптимизация) - после проверки на MVP
- Этап 10.1 (документация) - в процессе разработки

---

## Рекомендации для Claude Code

**При разработке следовать принципам:**
1. **Последовательность:** Завершать один модуль полностью, прежде чем переходить к следующему
2. **Тестирование:** Тестировать каждый модуль сразу после создания
3. **Минимализм:** Писать только необходимый код, избегать over-engineering
4. **Переиспользование:** Использовать общие интерфейсы и типы, избегать дублирования
5. **Безопасность:** Валидировать все входные данные, обрабатывать все ошибки
6. **Производительность:** Использовать object pooling, избегать аллокаций в hot path
7. **Читаемость:** Чистый код, понятные названия переменных и функций

**Порядок реализации коннекторов:**
- Начать с Bybit (самый детальный)
- Затем Bitget (для MVP)
- Остальные биржи добавлять по одной, копируя структуру Bybit

**При возникновении проблем:**
- Логировать подробно все ошибки
- Использовать retry с exponential backoff для сетевых запросов
- Graceful degradation (например, пауза пары при ошибке, а не падение всего бота)

---

**Версия плана:** 1.0
**Дата создания:** 2025-12-04
**Последнее обновление:** 2025-12-04
