# Требования к торговому ядру

## 1. Производительность

### Целевые метрики латентности

| Операция | Целевое значение | Критическое значение | Примечание |
|----------|------------------|----------------------|------------|
| **Tick → Order** (без сети) | < 5ms | < 10ms | От получения WebSocket до отправки ордера |
| **Расчёт спреда** | < 0.5ms | < 1ms | Арифметика + учёт комиссий |
| **Парсинг WebSocket JSON** | < 0.1ms | < 0.5ms | Использовать `jsoniter` вместо стандартного |
| **Обработка PriceUpdate** | < 1ms | < 2ms | Парсинг + валидация + dispatch |
| **Пересчёт лучших цен** | O(k) | O(k) | k = число бирж (6), < 0.1ms |

### Архитектурные требования

- **Event-driven архитектура:** Мгновенная реакция на WebSocket события (не polling)
- **Шардирование по символам:** Разные пары обрабатываются независимо
- **Lock-free чтение:** `sync.Map` для concurrent read/write, `atomic` для счётчиков
- **Параллельная отправка ордеров:** Два ордера на разные биржи отправляются одновременно
- **Zero-allocation в hot path:** `sync.Pool` для переиспользования объектов

### Требования к памяти

- **Object pooling:** PriceUpdate, BestPrices, OrderParams
- **Предаллокация:** Слайсы с известной capacity, буферизованные каналы
- **In-place обновление:** Мутабельные структуры где безопасно

---

## 2. Rate Limits бирж

### Общая стратегия

- Использовать **50% от максимального лимита** для безопасности
- WebSocket не считается в rate limit (подключились один раз)
- REST запросы: выставление ордеров, проверка баланса, установка плеча

### Детальные лимиты

| Биржа | REST лимит (оригинал) | Безопасный лимит | Примечание |
|-------|----------------------|------------------|------------|
| **Bybit** | 120 req/min (trade) | 60 req/min | По UID, разделено на trade/read |
| **Bitget** | 100 req/min (per IP) | 50 req/min | Per-minute и per-IP |
| **BingX** | 10 req/sec (order placement) | 5 req/sec | 2000 req/10sec общий лимит |
| **Gate.io** | 10 req/10sec (при низком fill ratio) | 5 req/10sec | Динамическая система, зависит от fill ratio |
| **OKX** | 1000 req/2sec (sub-account) | 500 req/2sec | По sub-account, только новые/измененные ордера |
| **HTX** | 72 req/3sec (trade), 144 req/3sec (query) | 36 req/3sec | По UID, разделено на trade/query |
| **MEXC** | 20 req/2sec (market data) | 10 req/2sec | Per IP |

### Типичная нагрузка от бота

**Для одной пары:**
- Установка плеча (первый раз): 2 запроса (по одному на каждую биржу)
- Проверка баланса перед входом: 2 запроса
- Вход 1 часть (N=1): 2 ордера = 2 запроса
- Проверка статуса ордеров: 0 запросов (получаем через WebSocket)
- Выход 1 часть: 2 ордера = 2 запроса

**Итого:** ~8 REST запросов на полный цикл арбитража (1 вход + 1 выход)

**Для 30 пар с 2 одновременными арбитражами:**
- В худшем случае: 2 арбитража × 8 запросов = 16 запросов/цикл
- Цикл арбитража: ~5-30 минут
- **Средняя нагрузка:** ~1-3 REST запроса/минуту на биржу

**Вывод:** Даже при самых строгих лимитах (Gate.io: 5 req/10sec) бот укладывается с большим запасом.

---

## 3. Комиссии бирж (Taker)

Все рыночные ордера считаются **Taker** (забирают ликвидность).

| Биржа | Maker | Taker | Примечание |
|-------|-------|-------|------------|
| **Bybit** | 0.020% | 0.055% | Стандартные ставки |
| **Bitget** | 0.020% | 0.060% | Futures стандарт |
| **BingX** | 0.020% | 0.050% | Perpetual futures |
| **Gate.io** | 0.020% | 0.050% | Базовые ставки |
| **OKX** | 0.020% | 0.050% | Perpetual swap |
| **HTX** | 0.020% | 0.040% | Наименьшая комиссия |
| **MEXC** | 0.010% | 0.040% | Наименьший Maker |

### Расчёт чистого спреда

```
Чистый спред = Спред (%) - 2 × (комиссия_биржа_A + комиссия_биржа_B)
```

**Пример:** Bybit (long) + Bitget (short)
```
Комиссии на вход: 0.055% + 0.060% = 0.115%
Комиссии на выход: 0.055% + 0.060% = 0.115%
Итого: 2 × 0.115% = 0.23%

Если спред = 1.2%, чистый спред = 1.2% - 0.23% = 0.97%
```

### Хранение комиссий в коде

```go
// internal/exchanges/fees.go
var TakerFees = map[string]float64{
    "bybit":  0.00055,
    "bitget": 0.00060,
    "bingx":  0.00050,
    "gate":   0.00050,
    "okx":    0.00050,
    "htx":    0.00040,
    "mexc":   0.00040,
}
```

---

## 4. Минимальные объёмы и размеры лотов

### Получение через API

При старте бота делаем запросы:

| Биржа | Endpoint | Параметры |
|-------|----------|-----------|
| **Bybit** | `GET /v5/market/instruments-info` | `category=linear` |
| **Bitget** | `GET /api/mix/v1/market/contracts` | `productType=umcbl` |
| **BingX** | `GET /openApi/swap/v2/quote/contracts` | - |
| **Gate.io** | `GET /api/v4/futures/usdt/contracts` | - |
| **OKX** | `GET /api/v5/public/instruments` | `instType=SWAP` |
| **HTX** | `GET /linear-swap-api/v1/swap_contract_info` | - |
| **MEXC** | `GET /api/v1/contract/detail` | - |

### Что получаем

Для каждого символа:
- `minQty` — минимальный объём ордера (в монетах актива)
- `tickSize` — шаг цены
- `lotSize` — шаг объёма

**Пример ответа (Bybit):**
```json
{
  "symbol": "BTCUSDT",
  "lotSizeFilter": {
    "minOrderQty": "0.001",
    "maxOrderQty": "500",
    "qtyStep": "0.001"
  },
  "priceFilter": {
    "tickSize": "0.5"
  }
}
```

### Кэширование

- Загружаем минимумы при старте бота
- Сохраняем в памяти (in-memory map)
- Обновляем раз в сутки (биржи редко меняют лимиты)

### Проверка при добавлении пары

```go
func ValidatePairVolume(symbol string, volume float64) error {
    for _, exchange := range connectedExchanges {
        minQty := getMinQty(exchange, symbol)
        if volume < minQty {
            return fmt.Errorf("объём %.4f меньше минимума на %s (%.4f)",
                              volume, exchange, minQty)
        }
    }
    return nil
}
```

---

## 5. Плечо (Leverage)

### Установка плеча

Бот автоматически выставляет плечо перед первым входом по паре.

| Биржа | Endpoint | Примечание |
|-------|----------|------------|
| **Bybit** | `POST /v5/position/set-leverage` | На каждый символ отдельно |
| **Bitget** | `POST /api/mix/v1/account/setLeverage` | `symbol`, `marginCoin`, `leverage` |
| **BingX** | `POST /openApi/swap/v2/trade/leverage` | `symbol`, `leverage`, `side` |
| **Gate.io** | `POST /api/v4/futures/usdt/positions/{contract}/leverage` | В URL контракт |
| **OKX** | `POST /api/v5/account/set-leverage` | `instId`, `lever`, `mgnMode` |
| **HTX** | `POST /linear-swap-api/v1/swap_switch_lever_rate` | `contract_code`, `lever_rate` |
| **MEXC** | `POST /api/v1/private/position/change_leverage` | `symbol`, `leverage`, `openType` |

### Кэширование установленного плеча

```go
// Сохраняем в БД после первой установки
type LeverageCache struct {
    Exchange string
    Symbol   string
    Leverage int
    UpdatedAt time.Time
}
```

**Проверка:**
- Если плечо уже установлено для пары на бирже → не делаем запрос повторно
- Проверяем плечо только при изменении параметра пользователем

---

## 6. Orderbook глубина

Для расчёта средней цены исполнения учитываем **10 уровней** стакана.

### WebSocket подписки

| Биржа | Channel | Формат |
|-------|---------|--------|
| **Bybit** | `orderbook.{depth}.{symbol}` | `orderbook.50.BTCUSDT` |
| **Bitget** | `books` | JSON с биржевым форматом |
| **BingX** | `{symbol}@depth` | `BTC-USDT@depth` |
| **Gate.io** | `futures.order_book` | `{"channel": "futures.order_book", "payload": ["BTC_USDT", "20"]}` |
| **OKX** | `books5` или `books` | `{"channel":"books5","instId":"BTC-USDT-SWAP"}` |
| **HTX** | `market.{symbol}.depth.step0` | `market.BTC-USDT.depth.step0` |
| **MEXC** | `sub.depth` | Кастомный формат |

### Расчёт средней цены

```go
func CalculateAvgPrice(orderbook []Level, volume float64, side string) float64 {
    var totalCost, totalQty float64
    var levels []Level

    if side == "buy" {
        levels = orderbook.Asks // Покупаем по аскам
    } else {
        levels = orderbook.Bids // Продаём по бидам
    }

    for _, level := range levels[:10] { // Берём 10 уровней
        qty := math.Min(level.Quantity, volume - totalQty)
        totalCost += level.Price * qty
        totalQty += qty

        if totalQty >= volume {
            break
        }
    }

    return totalCost / totalQty
}
```

---

## 7. Лимит одновременных арбитражей

**По умолчанию:** 2 арбитража максимум

**Настройка:**
- Глобальный параметр в конфиге
- Можно менять на лету через API
- При превышении лимита новые арбитражи ждут освобождения слота

**Логика:**
```go
type ArbitrageSlots struct {
    MaxSlots int
    ActiveSlots map[string]bool // symbol -> active
    mu sync.RWMutex
}

func (a *ArbitrageSlots) CanStart(symbol string) bool {
    a.mu.RLock()
    defer a.mu.RUnlock()
    return len(a.ActiveSlots) < a.MaxSlots
}
```

---

## 8. Задержки между частями входа/выхода

**Нет фиксированной задержки.**

**Логика:**
- После каждой части: проверяем актуальный спред
- Если спред >= порога → входим следующей частью сразу
- Если спред < порога → ждём
- Приоритет: **скорость** (не упустить спред)

**Обновление стакана:**
- WebSocket присылает обновления каждые 100-300ms
- Достаточно быстро для принятия решения о следующей части

---

## 9. Метрики и мониторинг

### Метрики производительности (Prometheus-compatible)

| Метрика | Тип | Bucket'ы / Labels |
|---------|-----|-------------------|
| `tick_to_order_duration_ms` | Histogram | 0.1, 0.5, 1, 5, 10, 50, 100 |
| `spread_calculation_duration_ms` | Histogram | 0.1, 0.5, 1 |
| `order_execution_duration_ms` | Histogram | 50, 100, 200, 500, 1000 |
| `websocket_events_total` | Counter | exchange, symbol, event_type |
| `orders_total` | Counter | exchange, symbol, side, status |
| `arbitrage_cycles_total` | Counter | symbol, result (success/sl/error) |
| `buffer_overflow_total` | Counter | shard_id |

### Логирование

**Уровни:**
- `DEBUG`: Каждое событие WebSocket
- `INFO`: Открытие/закрытие позиций, изменение состояний
- `WARN`: Retry попытки, близость к rate limit
- `ERROR`: Ошибки API, неудачные ордера, ликвидации

**Формат:** JSON для удобного парсинга
```json
{
  "timestamp": "2025-12-05T15:30:45Z",
  "level": "INFO",
  "event": "arbitrage_opened",
  "symbol": "BTCUSDT",
  "exchanges": ["bybit", "bitget"],
  "spread": 1.15,
  "volume": 0.5
}
```

---

## 10. Конфигурируемые параметры

| Параметр | Значение по умолчанию | Диапазон | Описание |
|----------|----------------------|----------|----------|
| `buffer_size_per_shard` | 2000 | 100-10000 | Размер буфера канала на шард |
| `num_shards` | `runtime.NumCPU()` | 4-32 | Количество шардов для параллелизма |
| `workers_per_shard` | 2 | 1-8 | Workers на каждый шард |
| `balance_update_interval` | 60s | 30s-300s | Как часто обновлять балансы |
| `stats_update_interval` | 60s | 10s-300s | Как часто обновлять статистику |
| `max_concurrent_arbitrages` | 2 | 1-10 | Лимит одновременных арбитражей |
| `orderbook_depth` | 10 | 5-20 | Сколько уровней стакана учитывать |
| `reconnect_initial_delay` | 2s | 1s-10s | Начальная задержка при reconnect |
| `reconnect_max_retries` | 5 | 3-10 | Максимум попыток переподключения |

---

**Следующий документ:** После того как скопируешь этот, скажи "готово" и я дам следующий — **TZ.md** (техническое задание только на торговое ядро).
