# Архитектура торгового ядра

## Общая структура проекта

```
arbitrage-terminal/
├── cmd/
│   └── trading-engine/
│       └── main.go                    # Точка входа
│
├── internal/
│   ├── core/                          # Ядро торговой логики
│   │   ├── engine/
│   │   │   ├── engine.go
│   │   │   └── coordinator.go
│   │   ├── arbitrage/
│   │   │   ├── pair.go
│   │   │   ├── entry.go
│   │   │   ├── exit.go
│   │   │   └── monitor.go
│   │   ├── prices/
│   │   │   ├── aggregator.go
│   │   │   └── tracker.go
│   │   └── orderbook/
│   │       └── orderbook.go
│   │
│   ├── exchanges/                     # Коннекторы бирж
│   │   ├── common.go
│   │   ├── bybit/
│   │   │   ├── bybit.go
│   │   │   ├── websocket.go
│   │   │   ├── rest.go
│   │   │   ├── parser.go
│   │   │   └── types.go
│   │   ├── bitget/
│   │   ├── bingx/
│   │   ├── gate/
│   │   ├── okx/
│   │   ├── htx/
│   │   └── mexc/
│   │
│   ├── state/                         # State machine
│   │   ├── machine.go
│   │   └── transitions.go
│   │
│   ├── risk/                          # Управление рисками
│   │   ├── balance.go
│   │   ├── stoploss.go
│   │   └── limits.go
│   │
│   ├── db/                            # Работа с БД
│   │   ├── models.go
│   │   ├── repository.go
│   │   └── migrations/
│   │       ├── 001_create_pairs.up.sql
│   │       ├── 001_create_pairs.down.sql
│   │       ├── 002_create_trades.up.sql
│   │       ├── 002_create_trades.down.sql
│   │       ├── 003_create_leverage_cache.up.sql
│   │       └── 003_create_leverage_cache.down.sql
│   │
│   └── config/                        # Конфигурация
│       ├── config.go
│       └── fees.go
│
├── pkg/                               # Переиспользуемые пакеты
│   ├── ratelimit/
│   │   └── limiter.go
│   ├── pool/
│   │   └── object_pool.go
│   └── metrics/
│       └── prometheus.go
│
├── configs/
│   ├── exchanges.json                 # Минимумы, лимиты бирж
│   └── app.yaml                       # Основная конфигурация
│
├── go.mod
└── go.sum
```

---

## Краткая сводка всех файлов

### 1. Точка входа

| Файл | Назначение | Ключевые функции |
|------|-----------|------------------|
| `cmd/trading-engine/main.go` | Запуск приложения, инициализация компонентов, graceful shutdown | `main()`, `initConfig()`, `initDB()`, `initExchanges()` |

---

### 2. Ядро торговой логики

| Файл | Назначение | Ключевые типы/функции |
|------|-----------|----------------------|
| `internal/core/engine/engine.go` | Главный координатор системы, управление парами | `TradingEngine`, `ArbitrageSlots`, `AddPair()`, `Start()` |
| `internal/core/engine/coordinator.go` | ⚠️ Шардирование событий по символам (см. детали ниже) | `Coordinator`, `Shard`, `RouteEvent()` |
| `internal/core/arbitrage/pair.go` | ⚠️ Координация арбитража для одной пары (см. детали ниже) | `Pair`, `Position`, `OnPriceUpdate()` |
| `internal/core/arbitrage/entry.go` | ⚠️ Логика входа в позицию частями (см. детали ниже) | `enterPosition()`, `enterPart()`, `retrySecondLeg()` |
| `internal/core/arbitrage/exit.go` | ⚠️ Логика выхода + плавающее состояние (см. детали ниже) | `exitPosition()`, `exitPart()`, `calculateRealizedPNL()` |
| `internal/core/arbitrage/monitor.go` | ⚠️ Мониторинг открытой позиции (см. детали ниже) | `monitorPosition()`, `updateUnrealizedPNL()`, `checkStopLoss()` |
| `internal/core/prices/aggregator.go` | Сбор цен со всех бирж через WebSocket | `Aggregator`, `Subscribe()`, `handlePriceUpdate()` |
| `internal/core/prices/tracker.go` | Отслеживание лучших цен для символа | `Tracker`, `GetBestPrices()`, `CalculateAvgPrice()` |
| `internal/core/orderbook/orderbook.go` | Работа со стаканом (расчёт средней цены) | `OrderBook`, `Level`, `CalculateAvgPrice()` |

---

### 3. Коннекторы бирж

| Файл | Назначение | Ключевые типы/функции |
|------|-----------|----------------------|
| `internal/exchanges/common.go` | Общий интерфейс для всех бирж | `Exchange`, `OrderRequest`, `OrderResponse`, `Balance` |
| `internal/exchanges/bybit/bybit.go` | Главный клиент Bybit | `Client`, `Connect()`, `PlaceOrder()`, `GetBalance()` |
| `internal/exchanges/bybit/websocket.go` | ⚠️ WebSocket клиент + reconnect логика (см. детали ниже) | `WebSocketClient`, `reconnect()`, `handleMessages()` |
| `internal/exchanges/bybit/rest.go` | REST API запросы к Bybit | `placeOrderREST()`, `getBalanceREST()`, `doRequest()` |
| `internal/exchanges/bybit/parser.go` | Парсинг JSON ответов Bybit | `parseOrderbookMessage()`, `parseOrderResponse()` |
| `internal/exchanges/bybit/types.go` | Специфичные типы данных Bybit | `BybitOrderResponse`, `BybitWebSocketMessage` |
| `internal/exchanges/{bitget,bingx,gate,okx,htx,mexc}/` | Аналогичная структура для каждой биржи | - |

---

### 4. State Machine

| Файл | Назначение | Ключевые типы/функции |
|------|-----------|----------------------|
| `internal/state/machine.go` | Управление состояниями пары | `Machine`, `State`, `Transition()`, `CanTransition()` |
| `internal/state/transitions.go` | Определение разрешённых переходов | `AllowedTransitions` (map) |

---

### 5. Управление рисками

| Файл | Назначение | Ключевые функции |
|------|-----------|------------------|
| `internal/risk/balance.go` | Проверка баланса перед входом | `CheckBalance()`, `CalculateRequiredMargin()` |
| `internal/risk/stoploss.go` | Проверка Stop Loss | `CheckStopLoss()`, `CalculatePNL()` |
| `internal/risk/limits.go` | Проверка минимальных объёмов | `ValidateVolume()`, `GetMinOrderSize()` |

---

### 6. База данных

| Файл | Назначение | Ключевые типы/функции |
|------|-----------|----------------------|
| `internal/db/models.go` | Модели данных | `PairConfig`, `Trade`, `LeverageCache` |
| `internal/db/repository.go` | CRUD операции | `CreatePair()`, `SaveTrade()`, `GetLeverage()` |
| `internal/db/migrations/*.sql` | SQL-скрипты для миграций | Схемы таблиц (см. ниже) |

---

### 7. Переиспользуемые пакеты

| Файл | Назначение | Ключевые типы/функции |
|------|-----------|----------------------|
| `pkg/ratelimit/limiter.go` | Rate limiting (Token Bucket) | `Limiter`, `Wait()` |
| `pkg/pool/object_pool.go` | Object pooling для переиспользования | `PriceUpdatePool`, `BestPricesPool` |
| `pkg/metrics/prometheus.go` | Метрики Prometheus | `TickToOrderDuration`, `OrdersTotal` |

---

### 8. Конфигурация

| Файл | Назначение | Содержание |
|------|-----------|-----------|
| `internal/config/config.go` | Загрузка конфигурации из YAML | `Config`, `Load()` |
| `internal/config/fees.go` | Комиссии бирж (захардкодить) | `TakerFees` (map) |
| `configs/app.yaml` | Основная конфигурация | БД, биржи, параметры движка |
| `configs/exchanges.json` | Минимумы и лимиты бирж | minQty, tickSize, rateLimits |

---

---

## Детальное описание ключевых модулей

### 🔥 1. `internal/core/engine/coordinator.go`

**Назначение:** Распределение событий по шардам для параллельной обработки.

**Ключевые типы:**
```go
type Coordinator struct {
    numShards   int
    shards      []*Shard
    router      *EventRouter
}

type Shard struct {
    id          int
    eventChan   chan *prices.PriceUpdate
    pairs       map[string]*arbitrage.Pair // symbol -> Pair
    workers     int
    mu          sync.RWMutex
}

type EventRouter struct {
    shardMap map[string]int // symbol -> shard_id
}
```

**Логика шардирования:**
```go
// При создании Coordinator
func NewCoordinator(numShards int, workersPerShard int) *Coordinator {
    c := &Coordinator{
        numShards: numShards,  // Обычно runtime.NumCPU()
        shards:    make([]*Shard, numShards),
    }

    // Создать N шардов
    for i := 0; i < numShards; i++ {
        c.shards[i] = &Shard{
            id:        i,
            eventChan: make(chan *prices.PriceUpdate, 1000),
            pairs:     make(map[string]*arbitrage.Pair),
            workers:   workersPerShard,
        }

        // Запустить workers для шарда
        for j := 0; j < workersPerShard; j++ {
            go c.shards[i].processEvents()
        }
    }

    return c
}

// Routing события к шарду
func (c *Coordinator) RouteEvent(event *prices.PriceUpdate) {
    // Hash-based routing
    shardID := hash(event.Symbol) % c.numShards
    c.shards[shardID].eventChan <- event
}

// Worker обрабатывает события
func (s *Shard) processEvents() {
    for event := range s.eventChan {
        s.mu.RLock()
        pair, exists := s.pairs[event.Symbol]
        s.mu.RUnlock()

        if exists {
            pair.OnPriceUpdate(event)
        }
    }
}
```

**Почему важно:**
- Lock-free чтение (каждый шард изолирован)
- Параллельная обработка до 180 потоков цен
- Латентность <5ms достигается через отсутствие contention

---

### 🔥 2. `internal/core/arbitrage/pair.go`

**Назначение:** Координация всего жизненного цикла арбитража для одной пары.

**Ключевые типы:**
```go
type Pair struct {
    id            int
    config        *db.PairConfig
    state         *state.Machine
    position      *Position
    priceTracker  *prices.Tracker
    riskManager   *risk.Manager
    exchanges     map[string]exchanges.Exchange
    balanceCache  map[string]float64  // exchange -> available margin
    mu            sync.RWMutex
}

type Position struct {
    ExchangeLong    string
    ExchangeShort   string
    EntryPriceLong  float64
    EntryPriceShort float64
    Volume          float64
    FilledParts     int
    TotalParts      int
    UnrealizedPNL   float64
    OpenedAt        time.Time
}
```

**Главный метод:**
```go
func (p *Pair) OnPriceUpdate(update *prices.PriceUpdate) {
    // Обновить трекер цен
    p.priceTracker.Update(update.Exchange, &prices.ExchangePrice{
        BestBid:   update.BestBid,
        BestAsk:   update.BestAsk,
        Orderbook: update.Orderbook,
        UpdatedAt: update.Timestamp,
    })

    currentState := p.state.CurrentState()

    switch currentState {
    case state.StateReady:
        // Проверить условия входа
        if p.checkEntryConditions() {
            p.state.Transition(state.StateEntering)
            go p.enterPosition()
        }

    case state.StatePositionOpen:
        // Обновить PNL
        p.updateUnrealizedPNL(p.priceTracker.GetBestPrices())

        // Проверить условия выхода
        if p.checkExitConditions() {
            p.state.Transition(state.StateExiting)
            go p.exitPosition()
        }
    }
}
```

**Проверка условий входа:**
```go
func (p *Pair) checkEntryConditions() bool {
    bestPrices := p.priceTracker.GetBestPrices()

    // 1. Спред >= порога
    if bestPrices.NetSpread < p.config.EntrySpread {
        return false
    }

    // 2. Есть свободный слот для арбитража
    if !p.engine.slots.TryAcquire(p.config.Symbol) {
        return false
    }

    // 3. Баланс достаточен (проверяем только для первой части)
    if p.position == nil || p.position.FilledParts == 0 {
        if err := p.checkBalanceFirstPart(); err != nil {
            p.engine.slots.Release(p.config.Symbol)
            return false
        }
    }

    return true
}
```

**Зависимости:**
- `internal/state` - управление состояниями
- `internal/core/prices` - данные цен
- `internal/exchanges` - выставление ордеров
- `internal/risk` - проверки рисков

---

### 🔥 3. `internal/core/arbitrage/entry.go`

**Назначение:** Логика входа в позицию частями с обработкой всех ошибок.

**Главный метод:**
```go
func (p *Pair) enterPosition() error {
    defer func() {
        if r := recover(); r != nil {
            log.Error("Panic in enterPosition", "error", r)
            p.state.Transition(state.StateError)
        }
    }()

    // 1. Установить плечо (если нужно)
    if err := p.setLeverageIfNeeded(); err != nil {
        p.state.Transition(state.StatePaused)
        return err
    }

    // 2. Проверить спред ещё раз (мог измениться после установки плеча)
    if !p.checkSpread("entry") {
        p.state.Transition(state.StateReady)
        p.engine.slots.Release(p.config.Symbol)
        return nil
    }

    // 3. Вход частями
    for i := p.position.FilledParts; i < p.config.NumOrders; i++ {
        // Проверить спред перед каждой частью
        if !p.checkSpread("entry") {
            // Проверить: может пора выходить?
            if p.checkSpread("exit") {
                p.state.Transition(state.StateExiting)
                return p.exitPosition()
            }
            // Спред недостаточен - ждём
            break
        }

        // Выставить часть
        if err := p.enterPart(i); err != nil {
            return p.handleEntryError(err, i)
        }

        p.position.FilledParts++
    }

    // Если открыли все части - переходим в POSITION_OPEN
    if p.position.FilledParts == p.config.NumOrders {
        p.state.Transition(state.StatePositionOpen)
        go p.monitorPosition()
    } else {
        // Частичный вход - остаёмся в ENTERING
        // Следующее обновление цены проверит условия снова
    }

    return nil
}
```

**Вход одной части:**
```go
func (p *Pair) enterPart(partIndex int) error {
    // Рассчитать объём части
    partVolume := p.calculatePartVolume(partIndex)

    // Выбрать биржи (дешёвую long, дорогую short)
    longExch, shortExch := p.selectExchanges()

    // Выставить ордера параллельно
    return p.executeOrdersPair(longExch, shortExch, partVolume)
}

func (p *Pair) executeOrdersPair(longExch, shortExch string, volume float64) error {
    var wg sync.WaitGroup
    var mu sync.Mutex
    var firstOrder, secondOrder *OrderResponse
    var firstErr, secondErr error

    // Long нога
    wg.Add(1)
    go func() {
        defer wg.Done()
        order, err := p.exchanges[longExch].PlaceOrder(&OrderRequest{
            Symbol:       p.config.Symbol,
            Side:         "buy",
            Type:         "market",
            Quantity:     volume,
            PositionSide: "long",
        })
        mu.Lock()
        firstOrder, firstErr = order, err
        mu.Unlock()
    }()

    // Short нога
    wg.Add(1)
    go func() {
        defer wg.Done()
        order, err := p.exchanges[shortExch].PlaceOrder(&OrderRequest{
            Symbol:       p.config.Symbol,
            Side:         "sell",
            Type:         "market",
            Quantity:     volume,
            PositionSide: "short",
        })
        mu.Lock()
        secondOrder, secondErr = order, err
        mu.Unlock()
    }()

    wg.Wait()

    // Проверить результаты
    if firstErr != nil && secondErr != nil {
        return fmt.Errorf("both legs failed: %v, %v", firstErr, secondErr)
    }

    if firstErr != nil {
        // Long не открылся, но short открылся - закрыть short
        return p.rollbackSecondLeg(secondOrder, shortExch)
    }

    if secondErr != nil {
        // Long открылся, но short не открылся - retry short
        return p.retrySecondLeg(firstOrder, shortExch, volume)
    }

    // Обе ноги успешны - обновить позицию
    p.updatePositionAfterEntry(firstOrder, secondOrder, longExch, shortExch)

    return nil
}
```

**Retry второй ноги:**
```go
func (p *Pair) retrySecondLeg(firstLeg *OrderResponse, secondExch string, volume float64) error {
    backoffs := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}

    for attempt := 0; attempt < 3; attempt++ {
        // Проверить спред
        if !p.checkSpread("entry") {
            // Спред ушёл - закрыть первую ногу
            return p.rollbackFirstLeg(firstLeg)
        }

        // Подождать
        time.Sleep(backoffs[attempt])

        // Попытка открыть вторую ногу
        secondOrder, err := p.exchanges[secondExch].PlaceOrder(&OrderRequest{
            Symbol:       p.config.Symbol,
            Side:         "sell",
            Type:         "market",
            Quantity:     volume,
            PositionSide: "short",
        })

        if err == nil {
            // Успех!
            p.updatePositionAfterEntry(firstLeg, secondOrder, firstLeg.Exchange, secondExch)
            return nil
        }

        // Если rate limit - подождать и ещё одна попытка
        if isRateLimitError(err) && attempt < 2 {
            time.Sleep(1 * time.Second)
            continue
        }

        // Если insufficient margin - сразу rollback
        if isInsufficientMarginError(err) {
            return p.rollbackFirstLeg(firstLeg)
        }
    }

    // Все попытки провалились - закрыть первую ногу
    return p.rollbackFirstLeg(firstLeg)
}

func (p *Pair) rollbackFirstLeg(order *OrderResponse) error {
    // Закрыть противоположным ордером
    closeSide := "sell"
    if order.Side == "sell" {
        closeSide = "buy"
    }

    _, err := p.exchanges[order.Exchange].PlaceOrder(&OrderRequest{
        Symbol:       p.config.Symbol,
        Side:         closeSide,
        Type:         "market",
        Quantity:     order.FilledQty,
        PositionSide: order.PositionSide,
    })

    if err != nil {
        log.Error("Failed to rollback first leg", "error", err)
        p.state.Transition(state.StateError)
    } else {
        p.state.Transition(state.StatePaused)
    }

    p.engine.slots.Release(p.config.Symbol)
    return err
}
```

**Установка плеча:**
```go
func (p *Pair) setLeverageIfNeeded() error {
    // Проверить кэш для каждой биржи
    for exchName := range p.exchanges {
        cached, exists, err := p.db.GetLeverage(exchName, p.config.Symbol)
        if err != nil {
            return err
        }

        if exists && cached == p.config.Leverage {
            continue  // Плечо уже установлено
        }

        // Установить плечо через REST
        if err := p.exchanges[exchName].SetLeverage(p.config.Symbol, p.config.Leverage); err != nil {
            return fmt.Errorf("failed to set leverage on %s: %w", exchName, err)
        }

        // Сохранить в кэш
        if err := p.db.SetLeverage(exchName, p.config.Symbol, p.config.Leverage); err != nil {
            log.Warn("Failed to cache leverage", "error", err)
        }
    }

    return nil
}
```

**Зависимости:**
- `internal/exchanges` - выставление ордеров
- `internal/db` - кэш плеча
- `internal/state` - переходы состояний

---

### 🔥 4. `internal/core/arbitrage/exit.go`

**Назначение:** Логика выхода из позиции + плавающее состояние (добор/выход).

**Главный метод:**
```go
func (p *Pair) exitPosition() error {
    defer func() {
        if r := recover(); r != nil {
            log.Error("Panic in exitPosition", "error", r)
            p.state.Transition(state.StateError)
        }
    }()

    partsToClose := p.position.FilledParts

    // Выход частями (зеркально входу)
    for i := 0; i < partsToClose; i++ {
        // Проверить спред перед каждой частью
        if p.checkSpread("exit") {
            // Условия выхода выполнены - закрыть часть
            if err := p.exitPart(i); err != nil {
                return p.handleExitError(err, i)
            }

            p.position.FilledParts--

        } else if p.checkSpread("entry") {
            // Спред расширился снова!
            // Плавающее состояние: можем добрать позицию
            if p.position.FilledParts < p.config.NumOrders {
                p.state.Transition(state.StateEntering)
                return p.enterPart(p.position.FilledParts)
            }
        } else {
            // Спред в промежуточной зоне - ждём
            break
        }
    }

    // Если закрыли все части - позиция завершена
    if p.position.FilledParts == 0 {
        p.finalizePosition()
        p.state.Transition(state.StateReady)
        p.engine.slots.Release(p.config.Symbol)
    } else {
        // Частичный выход - остаёмся в EXITING
        // Следующее обновление цены проверит условия снова
    }

    return nil
}
```

**Выход одной части:**
```go
func (p *Pair) exitPart(partIndex int) error {
    partVolume := p.calculatePartVolume(partIndex)

    var wg sync.WaitGroup
    var mu sync.Mutex
    var longErr, shortErr error

    // Закрыть long позицию (sell)
    wg.Add(1)
    go func() {
        defer wg.Done()
        _, err := p.exchanges[p.position.ExchangeLong].PlaceOrder(&OrderRequest{
            Symbol:       p.config.Symbol,
            Side:         "sell",
            Type:         "market",
            Quantity:     partVolume,
            PositionSide: "long",
        })
        mu.Lock()
        longErr = err
        mu.Unlock()
    }()

    // Закрыть short позицию (buy)
    wg.Add(1)
    go func() {
        defer wg.Done()
        _, err := p.exchanges[p.position.ExchangeShort].PlaceOrder(&OrderRequest{
            Symbol:       p.config.Symbol,
            Side:         "buy",
            Type:         "market",
            Quantity:     partVolume,
            PositionSide: "short",
        })
        mu.Lock()
        shortErr = err
        mu.Unlock()
    }()

    wg.Wait()

    if longErr != nil || shortErr != nil {
        return fmt.Errorf("exit failed: long=%v, short=%v", longErr, shortErr)
    }

    return nil
}
```

**Завершение позиции:**
```go
func (p *Pair) finalizePosition() {
    // Рассчитать реализованный PNL
    realizedPNL := p.calculateRealizedPNL()

    // Сохранить в историю сделок
    trade := &db.Trade{
        PairID:         p.id,
        EntryTime:      p.position.OpenedAt,
        ExitTime:       timePtr(time.Now()),
        EntrySpread:    p.position.EntrySpread,
        ExitSpread:     p.position.ExitSpread,
        RealizedPNL:    realizedPNL,
        ExchangeLong:   p.position.ExchangeLong,
        ExchangeShort:  p.position.ExchangeShort,
        Volume:         p.position.Volume,
        ClosedBy:       p.position.CloseReason,
    }

    if err := p.db.SaveTrade(trade); err != nil {
        log.Error("Failed to save trade", "error", err)
    }

    // Обнулить позицию
    p.position = nil

    log.Info("Position closed", "pnl", realizedPNL)
}

func (p *Pair) calculateRealizedPNL() float64 {
    // Simplified: не учитывает комиссии (можно добавить)
    longPNL := (p.position.ExitPriceLong - p.position.EntryPriceLong) * p.position.Volume
    shortPNL := (p.position.EntryPriceShort - p.position.ExitPriceShort) * p.position.Volume

    return longPNL + shortPNL
}
```

**Зависимости:**
- `internal/exchanges` - закрытие ордеров
- `internal/db` - сохранение истории
- `internal/state` - переходы состояний

---

### 🔥 5. `internal/core/arbitrage/monitor.go`

**Назначение:** Мониторинг открытой позиции (PNL, Stop Loss, условия выхода).

**Главный метод:**
```go
func (p *Pair) monitorPosition() {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            // Получить текущие цены
            bestPrices := p.priceTracker.GetBestPrices()

            // Обновить Unrealized PNL
            p.updateUnrealizedPNL(bestPrices)

            // Проверить Stop Loss
            if p.checkStopLoss() {
                log.Warn("Stop Loss triggered", "pnl", p.position.UnrealizedPNL)
                p.position.CloseReason = "stop_loss"
                p.state.Transition(state.StateExiting)
                p.exitPosition()
                return
            }

            // Проверить условия выхода (уже в pair.go через OnPriceUpdate)
            // Здесь дополнительная проверка на случай пропуска события

        case <-p.state.ShutdownCh:
            return
        }
    }
}
```

**Обновление PNL:**
```go
func (p *Pair) updateUnrealizedPNL(prices *prices.BestPrices) {
    p.mu.Lock()
    defer p.mu.Unlock()

    if p.position == nil {
        return
    }

    // Получить текущие цены для наших бирж
    currentLongPrice := p.priceTracker.GetPrice(p.position.ExchangeLong, "ask")
    currentShortPrice := p.priceTracker.GetPrice(p.position.ExchangeShort, "bid")

    // Long PNL
    pnlLong := (currentLongPrice - p.position.EntryPriceLong) * p.position.Volume

    // Short PNL
    pnlShort := (p.position.EntryPriceShort - currentShortPrice) * p.position.Volume

    p.position.UnrealizedPNL = pnlLong + pnlShort
}
```

**Проверка Stop Loss:**
```go
func (p *Pair) checkStopLoss() bool {
    if p.config.StopLoss == 0 {
        return false  // SL не задан
    }

    p.mu.RLock()
    pnl := p.position.UnrealizedPNL
    p.mu.RUnlock()

    return pnl <= -p.config.StopLoss
}
```

**Зависимости:**
- `internal/core/prices` - текущие цены
- `internal/state` - переход к выходу

---

### 🔥 6. `internal/exchanges/bybit/websocket.go`

**Назначение:** WebSocket клиент с автоматическим reconnect и обработкой разрывов.

**Ключевые типы:**
```go
type WebSocketClient struct {
    conn          *websocket.Conn
    url           string
    subscriptions map[string]bool  // channel -> subscribed
    callback      func(*PriceUpdate)
    reconnectCh   chan struct{}
    closeCh       chan struct{}
    pingTicker    *time.Ticker
    mu            sync.RWMutex
}
```

**Логика reconnect:**
```go
func (ws *WebSocketClient) Connect() error {
    conn, _, err := websocket.DefaultDialer.Dial(ws.url, nil)
    if err != nil {
        return err
    }

    ws.mu.Lock()
    ws.conn = conn
    ws.mu.Unlock()

    // Запустить обработчик сообщений
    go ws.handleMessages()

    // Запустить ping
    go ws.sendPing()

    return nil
}

func (ws *WebSocketClient) handleMessages() {
    for {
        _, message, err := ws.conn.ReadMessage()
        if err != nil {
            log.Error("WebSocket read error", "error", err)

            // Попытка reconnect
            ws.reconnectCh <- struct{}{}
            return
        }

        // Парсинг и callback
        update, err := parseOrderbookMessage(message)
        if err != nil {
            log.Warn("Failed to parse message", "error", err)
            continue
        }

        if ws.callback != nil {
            ws.callback(update)
        }
    }
}

func (ws *WebSocketClient) reconnect() {
    backoff := 2 * time.Second
    maxBackoff := 32 * time.Second
    maxRetries := 5

    for attempt := 1; attempt <= maxRetries; attempt++ {
        log.Info("Attempting reconnect", "attempt", attempt)

        time.Sleep(backoff)

        if err := ws.Connect(); err != nil {
            log.Error("Reconnect failed", "attempt", attempt, "error", err)

            // Exponential backoff
            backoff *= 2
            if backoff > maxBackoff {
                backoff = maxBackoff
            }

            continue
        }

        // Успешно переподключились - восстановить подписки
        ws.resubscribe()
        log.Info("Reconnected successfully")
        return
    }

    // Все попытки провалились
    log.Error("Failed to reconnect after max retries")

    // TODO: Уведомить Engine, что биржа недоступна
    // Engine должен перевести все пары с этой биржей в ПАУЗУ
}

func (ws *WebSocketClient) resubscribe() {
    ws.mu.RLock()
    channels := make([]string, 0, len(ws.subscriptions))
    for ch := range ws.subscriptions {
        channels = append(channels, ch)
    }
    ws.mu.RUnlock()

    for _, ch := range channels {
        if err := ws.subscribe(ch); err != nil {
            log.Error("Failed to resubscribe", "channel", ch, "error", err)
        }
    }
}
```

**Ping/Pong:**
```go
func (ws *WebSocketClient) sendPing() {
    ws.pingTicker = time.NewTicker(20 * time.Second)
    defer ws.pingTicker.Stop()

    for {
        select {
        case <-ws.pingTicker.C:
            ws.mu.Lock()
            if ws.conn != nil {
                if err := ws.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
                    log.Error("Ping failed", "error", err)
                    ws.mu.Unlock()
                    ws.reconnectCh <- struct{}{}
                    return
                }
            }
            ws.mu.Unlock()

        case <-ws.closeCh:
            return
        }
    }
}
```

**Обработка при открытой позиции:**
- Во время reconnect позиция **НЕ закрывается**
- После восстановления: продолжить мониторинг как обычно
- Если reconnect провалился: все пары с этой биржей → ПАУЗУ

**Зависимости:**
- `github.com/gorilla/websocket`

---

---

## Схемы таблиц БД

### Таблица `pairs`

```sql
CREATE TABLE pairs (
    id SERIAL PRIMARY KEY,
    symbol VARCHAR(20) NOT NULL,
    volume DECIMAL(20, 8) NOT NULL,
    entry_spread DECIMAL(10, 4) NOT NULL,
    exit_spread DECIMAL(10, 4) NOT NULL,
    num_orders INTEGER NOT NULL DEFAULT 1,
    stop_loss DECIMAL(20, 8),
    leverage INTEGER NOT NULL DEFAULT 1,
    status VARCHAR(20) NOT NULL DEFAULT 'PAUSED',
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_pairs_symbol ON pairs(symbol);
CREATE INDEX idx_pairs_status ON pairs(status);
```

---

### Таблица `trades`

```sql
CREATE TABLE trades (
    id SERIAL PRIMARY KEY,
    pair_id INTEGER NOT NULL REFERENCES pairs(id) ON DELETE CASCADE,
    entry_time TIMESTAMP NOT NULL,
    exit_time TIMESTAMP,
    entry_spread DECIMAL(10, 4) NOT NULL,
    exit_spread DECIMAL(10, 4),
    realized_pnl DECIMAL(20, 8),
    exchange_long VARCHAR(20) NOT NULL,
    exchange_short VARCHAR(20) NOT NULL,
    volume DECIMAL(20, 8) NOT NULL,
    closed_by VARCHAR(20),  -- 'target', 'stop_loss', 'liquidation', 'manual'
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_trades_pair_id ON trades(pair_id);
CREATE INDEX idx_trades_entry_time ON trades(entry_time);
```

---

### Таблица `leverage_cache`

```sql
CREATE TABLE leverage_cache (
    exchange VARCHAR(20) NOT NULL,
    symbol VARCHAR(20) NOT NULL,
    leverage INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (exchange, symbol)
);

CREATE INDEX idx_leverage_updated_at ON leverage_cache(updated_at);
```

---

---

## Конфигурационные файлы

### `configs/app.yaml`

```yaml
database:
  host: localhost
  port: 5432
  user: trading
  password: ${DB_PASSWORD}
  dbname: arbitrage
  max_connections: 20

engine:
  num_shards: 8               # runtime.NumCPU() или задать вручную
  workers_per_shard: 4
  buffer_size: 1000
  max_concurrent_arbs: 2
  balance_update_interval: 5m

exchanges:
  bybit:
    api_key: ${BYBIT_API_KEY}
    api_secret: ${BYBIT_API_SECRET}
    rest_url: https://api.bybit.com
    ws_url: wss://stream.bybit.com/v5/public/linear

  bitget:
    api_key: ${BITGET_API_KEY}
    api_secret: ${BITGET_API_SECRET}
    rest_url: https://api.bitget.com
    ws_url: wss://ws.bitget.com/mix/v1/stream

  bingx:
    api_key: ${BINGX_API_KEY}
    api_secret: ${BINGX_API_SECRET}
    rest_url: https://open-api.bingx.com
    ws_url: wss://open-api-swap.bingx.com/swap-market

  gate:
    api_key: ${GATE_API_KEY}
    api_secret: ${GATE_API_SECRET}
    rest_url: https://api.gateio.ws
    ws_url: wss://api.gateio.ws/ws/v4

  okx:
    api_key: ${OKX_API_KEY}
    api_secret: ${OKX_API_SECRET}
    passphrase: ${OKX_PASSPHRASE}
    rest_url: https://www.okx.com
    ws_url: wss://ws.okx.com:8443/ws/v5/public

  htx:
    api_key: ${HTX_API_KEY}
    api_secret: ${HTX_API_SECRET}
    rest_url: https://api.huobi.pro
    ws_url: wss://api.hbdm.com/swap-ws

  mexc:
    api_key: ${MEXC_API_KEY}
    api_secret: ${MEXC_API_SECRET}
    rest_url: https://contract.mexc.com
    ws_url: wss://contract.mexc.com/ws

logging:
  level: info               # debug, info, warn, error
  format: json
  output: /var/log/trading-engine.log

metrics:
  enabled: true
  port: 9090
  path: /metrics
```

---

### `configs/exchanges.json`

**Назначение:** Минимумы и лимиты для каждой биржи (обновляется раз в сутки через API).

```json
{
  "bybit": {
    "minOrderSizes": {
      "BTCUSDT": {
        "minQty": 0.001,
        "maxQty": 100.0,
        "qtyStep": 0.001,
        "tickSize": 0.5
      },
      "ETHUSDT": {
        "minQty": 0.01,
        "maxQty": 1000.0,
        "qtyStep": 0.01,
        "tickSize": 0.05
      }
    },
    "rateLimits": {
      "rest_per_minute": 120,
      "ws_subscriptions": 100
    }
  },

  "bitget": {
    "minOrderSizes": {
      "BTCUSDT": {
        "minQty": 0.001,
        "qtyStep": 0.001,
        "tickSize": 0.1
      }
    },
    "rateLimits": {
      "rest_per_minute": 60,
      "ws_subscriptions": 50
    }
  }

  // ... остальные биржи
}
```

**Обновление:** Скрипт запускается раз в сутки и обновляет этот файл через REST API каждой биржи.

---
