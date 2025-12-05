package bybit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"arbitrage-terminal/internal/exchanges"
	"arbitrage-terminal/pkg/metrics"

	"go.uber.org/zap"
)

// =============================================================================
// Константы таймаутов
// =============================================================================

const (
	// ConnectTimeout — таймаут подключения к бирже.
	// Уменьшен до 10s для быстрого failover (Requirements: Tick → Order < 5ms).
	ConnectTimeout = 10 * time.Second

	// OrderOperationTimeout — таймаут для операций с ордерами.
	// Критически важно для low-latency торговли.
	OrderOperationTimeout = 5 * time.Second

	// QueryOperationTimeout — таймаут для запросов данных.
	QueryOperationTimeout = 10 * time.Second
)

// =============================================================================
// Client — главный клиент Bybit
// =============================================================================

// Client реализует интерфейс exchanges.Exchange для работы с Bybit.
// Объединяет REST и WebSocket клиенты для полноценного взаимодействия с биржей.
//
// Основные возможности:
//   - Выставление и отмена ордеров (REST)
//   - Получение баланса и позиций (REST)
//   - Подписка на обновления стакана (Public WebSocket)
//   - Мониторинг позиций и ликвидаций (Private WebSocket)
//   - Автоматический reconnect при разрыве соединения
//
// Согласно TZ.md 2.5C:
// "Ликвидация: Если биржа ликвидировала одну позицию →
//  Немедленно закрыть вторую позицию"
//
// Пример использования:
//
//	client, err := bybit.NewClient(bybit.Config{
//	    APIKey:    "your-api-key",
//	    APISecret: "your-api-secret",
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	if err := client.Connect(); err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
//
//	// Публичный WebSocket для рыночных данных
//	client.SetPriceCallback(func(update *exchanges.PriceUpdate) {
//	    fmt.Printf("Price update: %+v\n", update)
//	    exchanges.PutPriceUpdateWithOrderBook(update)
//	})
//
//	// Приватный WebSocket для мониторинга ликвидаций
//	client.SetPositionCallback(func(event *bybit.PositionEvent) {
//	    if event.IsLiquidated {
//	        fmt.Printf("Liquidation detected: %s\n", event.Symbol)
//	    }
//	})
//
//	client.Subscribe("BTCUSDT", "ETHUSDT")
//
// Потокобезопасность:
//   - logger защищён через atomic.Value
//   - connected защищён через sync.RWMutex
type Client struct {
	rest      *RestClient              // REST клиент для торговых операций
	ws        *WebSocketClient         // Public WebSocket для рыночных данных
	privateWs *PrivateWebSocketClient  // Private WebSocket для позиций/ликвидаций
	logger    atomic.Value             // *zap.Logger (atomic для потокобезопасности)
	connected bool                     // Статус подключения
	mu        sync.RWMutex             // Защита состояния
}

// Config содержит конфигурацию для создания клиента Bybit.
type Config struct {
	// APIKey — ключ API Bybit (обязательный).
	APIKey string

	// APISecret — секретный ключ API Bybit (обязательный).
	APISecret string

	// RestBaseURL — базовый URL REST API (опционально, по умолчанию production).
	RestBaseURL string

	// WsURL — URL WebSocket сервера (опционально, по умолчанию production).
	WsURL string

	// RecvWindow — окно приёма запросов в миллисекундах (опционально).
	RecvWindow int

	// Timeout — таймаут HTTP запросов (опционально).
	Timeout time.Duration

	// Logger — логгер (опционально).
	Logger *zap.Logger
}

// NewClient создаёт новый клиент Bybit.
//
// Параметры:
//   - cfg: конфигурация клиента
//
// Возвращает ошибку, если API ключи не указаны.
//
// Пример:
//
//	client, err := bybit.NewClient(bybit.Config{
//	    APIKey:    os.Getenv("BYBIT_API_KEY"),
//	    APISecret: os.Getenv("BYBIT_API_SECRET"),
//	})
func NewClient(cfg Config) (*Client, error) {
	// Валидация
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("bybit: API key is required")
	}
	if cfg.APISecret == "" {
		return nil, fmt.Errorf("bybit: API secret is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	// Создаём REST клиент
	restClient, err := NewRestClient(RestClientConfig{
		BaseURL:    cfg.RestBaseURL,
		APIKey:     cfg.APIKey,
		APISecret:  cfg.APISecret,
		RecvWindow: cfg.RecvWindow,
		Timeout:    cfg.Timeout,
		Logger:     logger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create REST client: %w", err)
	}

	// Создаём Public WebSocket клиент
	// ВАЖНО: Callback НЕ передаётся здесь — будет установлен через SetPriceCallback
	// ДО подписки на символы. Это предотвращает потерю данных.
	wsClient, err := NewWebSocketClient(WebSocketConfig{
		URL:    cfg.WsURL,
		Logger: logger,
		// Callback: nil — будет установлен через SetPriceCallback
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create WebSocket client: %w", err)
	}

	// Создаём Private WebSocket клиент для мониторинга позиций и ликвидаций
	// Согласно TZ.md 2.5C: "Ликвидация: Если биржа ликвидировала одну позицию →
	// Немедленно закрыть вторую позицию"
	privateWsClient, err := NewPrivateWebSocketClient(PrivateWebSocketConfig{
		APIKey:    cfg.APIKey,
		APISecret: cfg.APISecret,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create private WebSocket client: %w", err)
	}

	client := &Client{
		rest:      restClient,
		ws:        wsClient,
		privateWs: privateWsClient,
	}
	client.logger.Store(logger)

	return client, nil
}

// getLogger возвращает текущий логгер (потокобезопасно).
func (c *Client) getLogger() *zap.Logger {
	if l := c.logger.Load(); l != nil {
		return l.(*zap.Logger)
	}
	return zap.NewNop()
}

// =============================================================================
// Реализация интерфейса exchanges.Exchange
// =============================================================================

// GetName возвращает название биржи.
func (c *Client) GetName() string {
	return ExchangeName
}

// Connect подключается к Bybit (REST проверка + WebSocket соединения).
//
// Метод выполняет:
//  1. Проверку доступности REST API (ping)
//  2. Установку Public WebSocket соединения (рыночные данные)
//  3. Установку Private WebSocket соединения (позиции, ликвидации)
//
// Возвращает ошибку, если подключение не удалось.
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil // Уже подключены
	}

	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()

	// Проверяем REST API
	c.getLogger().Info("connecting to Bybit REST API")
	if err := c.rest.Ping(ctx); err != nil {
		return fmt.Errorf("REST API ping failed: %w", err)
	}
	c.getLogger().Info("Bybit REST API is available")

	// Подключаем Public WebSocket (рыночные данные)
	c.getLogger().Info("connecting to Bybit Public WebSocket")
	if err := c.ws.Connect(); err != nil {
		return fmt.Errorf("public WebSocket connection failed: %w", err)
	}
	c.getLogger().Info("Bybit Public WebSocket connected")

	// Подключаем Private WebSocket (позиции, ликвидации)
	// Согласно TZ.md 2.5C для мониторинга ликвидаций
	c.getLogger().Info("connecting to Bybit Private WebSocket")
	if err := c.privateWs.Connect(); err != nil {
		// Private WS не критичен — логируем предупреждение, но не прерываем
		c.getLogger().Warn("private WebSocket connection failed (liquidation monitoring unavailable)",
			zap.Error(err),
		)
	} else {
		c.getLogger().Info("Bybit Private WebSocket connected")
	}

	c.connected = true
	return nil
}

// Close закрывает все соединения с Bybit.
//
// Закрывает в порядке:
//  1. Private WebSocket (позиции, ликвидации)
//  2. Public WebSocket (рыночные данные)
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil
	}

	c.getLogger().Info("closing Bybit connections")

	// Закрываем Private WebSocket
	if c.privateWs != nil {
		if err := c.privateWs.Close(); err != nil {
			c.getLogger().Warn("error closing private WebSocket", zap.Error(err))
		}
	}

	// Закрываем Public WebSocket
	if err := c.ws.Close(); err != nil {
		c.getLogger().Warn("error closing public WebSocket", zap.Error(err))
	}

	c.connected = false
	c.getLogger().Info("Bybit connections closed")

	return nil
}

// PlaceOrder выставляет ордер на бирже.
//
// Параметры:
//   - req: параметры ордера (символ, сторона, тип, объём, цена)
//
// Возвращает информацию о созданном ордере или ошибку.
//
// Пример:
//
//	resp, err := client.PlaceOrder(&exchanges.OrderRequest{
//	    Symbol:       "BTCUSDT",
//	    Side:         exchanges.OrderSideBuy,
//	    Type:         exchanges.OrderTypeMarket,
//	    Quantity:     0.001,
//	    PositionSide: exchanges.PositionSideLong,
//	})
func (c *Client) PlaceOrder(req *exchanges.OrderRequest) (*exchanges.OrderResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("order request is nil")
	}

	// Используем короткий таймаут для критических операций
	ctx, cancel := context.WithTimeout(context.Background(), OrderOperationTimeout)
	defer cancel()

	// Запускаем таймер для метрики tick-to-order
	timer := metrics.NewTimer()

	// Конвертируем запрос в формат Bybit
	bybitReq := &PlaceOrderRequest{
		Category:    CategoryLinear,
		Symbol:      req.Symbol,
		Side:        ToBybitSide(req.Side),
		OrderType:   ToBybitOrderType(req.Type),
		Qty:         FormatFloat(req.Quantity, 8), // 8 знаков после запятой для объёма
		PositionIdx: ToBybitPositionIdx(req.PositionSide),
	}

	// Добавляем цену для лимитных ордеров
	if req.Type == exchanges.OrderTypeLimit && req.Price != nil {
		bybitReq.Price = FormatFloat(*req.Price, 8) // 8 знаков после запятой для цены
		bybitReq.TimeInForce = TIFGoodTillCancel
	}

	// Выставляем ордер
	result, err := c.rest.PlaceOrder(ctx, bybitReq)
	if err != nil {
		return nil, err
	}

	// Записываем метрику tick-to-order (если это было быстрое исполнение после тика)
	metrics.ObserveTickToOrder(ExchangeName, req.Symbol, timer.ElapsedMs())

	// Конвертируем результат в общий формат
	return ParsePlaceOrderResult(result, bybitReq), nil
}

// GetBalance возвращает баланс указанного актива.
//
// Параметры:
//   - asset: название актива (например, "USDT")
//
// Пример:
//
//	balance, err := client.GetBalance("USDT")
//	fmt.Printf("Available: %.2f, Total: %.2f\n", balance.Available, balance.Total)
func (c *Client) GetBalance(asset string) (*exchanges.Balance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	coinInfo, err := c.rest.GetBalance(ctx, asset)
	if err != nil {
		return nil, err
	}

	return ParseBalance(coinInfo)
}

// SetLeverage устанавливает плечо для указанного символа.
//
// Параметры:
//   - symbol: символ торговой пары (например, "BTCUSDT")
//   - leverage: значение плеча (например, 10)
//
// Примечание: если плечо уже установлено на указанное значение,
// метод возвращает nil без ошибки.
func (c *Client) SetLeverage(symbol string, leverage int) error {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	return c.rest.SetLeverage(ctx, symbol, leverage)
}

// =============================================================================
// Дополнительные методы REST
// =============================================================================

// PlaceOrderAndWait выставляет ордер и ожидает его исполнения.
//
// Согласно TZ.md: "Дождаться исполнения обоих [ордеров]"
//
// Параметры:
//   - req: параметры ордера
//   - timeout: максимальное время ожидания исполнения
//
// Возвращает информацию об исполненном ордере или ошибку.
// Для market ордеров ожидание обычно минимально (< 100ms).
//
// Пример:
//
//	resp, err := client.PlaceOrderAndWait(&exchanges.OrderRequest{
//	    Symbol: "BTCUSDT",
//	    Side:   exchanges.OrderSideBuy,
//	    Type:   exchanges.OrderTypeMarket,
//	    Quantity: 0.001,
//	}, 5*time.Second)
func (c *Client) PlaceOrderAndWait(req *exchanges.OrderRequest, timeout time.Duration) (*exchanges.OrderResponse, error) {
	// Выставляем ордер
	result, err := c.PlaceOrder(req)
	if err != nil {
		return nil, err
	}

	// Для market ордеров ждём исполнения
	if req.Type == exchanges.OrderTypeMarket {
		filled, err := c.WaitOrderFilled(req.Symbol, result.OrderID, timeout)
		if err != nil {
			// Возвращаем результат с предупреждением, но не ошибку
			// Ордер уже выставлен, возможно исполнен
			c.getLogger().Warn("не удалось подтвердить исполнение ордера",
				zap.String("orderId", result.OrderID),
				zap.Error(err),
			)
			return result, nil
		}
		return filled, nil
	}

	return result, nil
}

// WaitOrderFilled ожидает исполнения ордера с polling.
//
// Параметры:
//   - symbol: символ торговой пары
//   - orderID: ID ордера
//   - timeout: максимальное время ожидания
//
// Возвращает информацию об исполненном ордере или ошибку по таймауту.
//
// Интервал polling: 100ms (оптимально для market ордеров).
func (c *Client) WaitOrderFilled(symbol, orderID string, timeout time.Duration) (*exchanges.OrderResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Интервал polling — 100ms достаточно для market ордеров
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("таймаут ожидания исполнения ордера %s", orderID)
		case <-ticker.C:
			orderInfo, err := c.rest.GetOrderByID(ctx, symbol, orderID)
			if err != nil {
				// Продолжаем попытки при временных ошибках
				c.getLogger().Debug("ошибка получения статуса ордера, повторяем",
					zap.String("orderId", orderID),
					zap.Error(err),
				)
				continue
			}

			// Проверяем статус ордера
			switch orderInfo.OrderStatus {
			case OrderStatusFilled:
				// Ордер полностью исполнен
				c.getLogger().Debug("ордер исполнен",
					zap.String("orderId", orderID),
					zap.String("status", orderInfo.OrderStatus),
				)
				return ParseOrderResponse(orderInfo)

			case OrderStatusPartiallyFilled:
				// Частичное исполнение — продолжаем ждать
				c.getLogger().Debug("ордер частично исполнен, ожидаем",
					zap.String("orderId", orderID),
					zap.String("filledQty", orderInfo.CumExecQty),
				)
				continue

			case OrderStatusCancelled, OrderStatusDeactivated, OrderStatusRejected:
				// Ордер отменён или отклонён
				return nil, fmt.Errorf("ордер %s в статусе %s", orderID, orderInfo.OrderStatus)

			case OrderStatusNew:
				// Ордер ещё не исполнен — продолжаем ждать
				continue

			default:
				// Неизвестный статус — продолжаем ждать
				c.getLogger().Debug("неизвестный статус ордера",
					zap.String("orderId", orderID),
					zap.String("status", orderInfo.OrderStatus),
				)
				continue
			}
		}
	}
}

// GetOrderStatus возвращает текущий статус ордера.
//
// Параметры:
//   - symbol: символ торговой пары
//   - orderID: ID ордера
//
// Возвращает информацию о ордере в общем формате.
func (c *Client) GetOrderStatus(symbol, orderID string) (*exchanges.OrderResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	orderInfo, err := c.rest.GetOrderByID(ctx, symbol, orderID)
	if err != nil {
		return nil, err
	}

	return ParseOrderResponse(orderInfo)
}

// CancelOrder отменяет ордер на бирже.
//
// Параметры:
//   - symbol: символ торговой пары
//   - orderID: ID ордера для отмены
func (c *Client) CancelOrder(symbol, orderID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), OrderOperationTimeout)
	defer cancel()

	return c.rest.CancelOrder(ctx, symbol, orderID)
}

// GetPositions возвращает список открытых позиций.
//
// Параметры:
//   - symbol: символ торговой пары (пустая строка = все позиции)
func (c *Client) GetPositions(symbol string) (*GetPositionsResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	return c.rest.GetPositions(ctx, symbol)
}

// GetPosition возвращает позицию по указанному символу.
//
// Параметры:
//   - symbol: символ торговой пары
//
// Возвращает nil, nil если позиция не найдена.
func (c *Client) GetPosition(symbol string) (*PositionInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	return c.rest.GetPosition(ctx, symbol)
}

// GetOrders возвращает список активных ордеров.
//
// Параметры:
//   - symbol: символ торговой пары (пустая строка = все ордера)
func (c *Client) GetOrders(symbol string) (*GetOrdersResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	return c.rest.GetOrders(ctx, symbol)
}

// GetInstrumentInfo возвращает информацию о торговом инструменте.
//
// Параметры:
//   - symbol: символ торговой пары
//
// Возвращает информацию о минимумах, шагах цены/объёма и т.д.
func (c *Client) GetInstrumentInfo(symbol string) (*InstrumentInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	return c.rest.GetInstrumentInfo(ctx, symbol)
}

// GetInstrumentLimits возвращает лимиты инструмента в общем формате.
//
// Параметры:
//   - symbol: символ торговой пары
func (c *Client) GetInstrumentLimits(symbol string) (*exchanges.InstrumentLimits, error) {
	info, err := c.GetInstrumentInfo(symbol)
	if err != nil {
		return nil, err
	}

	return ParseInstrumentLimits(info)
}

// GetWalletBalance возвращает полный баланс кошелька.
func (c *Client) GetWalletBalance() (*GetWalletBalanceResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	return c.rest.GetWalletBalance(ctx)
}

// =============================================================================
// Методы WebSocket
// =============================================================================

// Subscribe подписывается на обновления стакана для указанных символов.
//
// Параметры:
//   - symbols: список символов (например, "BTCUSDT", "ETHUSDT")
//
// Пример:
//
//	client.Subscribe("BTCUSDT", "ETHUSDT", "XRPUSDT")
func (c *Client) Subscribe(symbols ...string) error {
	return c.ws.Subscribe(symbols...)
}

// SubscribeTickers подписывается на тикеры для указанных символов.
//
// Тикеры содержат только лучшие цены bid/ask без полного стакана,
// что даёт меньшую нагрузку на парсинг.
func (c *Client) SubscribeTickers(symbols ...string) error {
	return c.ws.SubscribeTickers(symbols...)
}

// Unsubscribe отписывается от обновлений для указанных символов.
func (c *Client) Unsubscribe(symbols ...string) error {
	return c.ws.Unsubscribe(symbols...)
}

// SetPriceCallback устанавливает callback для получения обновлений цен.
//
// Callback вызывается для каждого обновления стакана или тикера.
// ВАЖНО:
//   - Callback выполняется в горутине WebSocket, не блокируйте его
//   - PriceUpdate получен из пула, после обработки верните его:
//     exchanges.PutPriceUpdateWithOrderBook(update)
//
// Пример:
//
//	client.SetPriceCallback(func(update *exchanges.PriceUpdate) {
//	    aggregator.HandleUpdate(update)
//	    // После обработки вернуть в пул
//	    exchanges.PutPriceUpdateWithOrderBook(update)
//	})
func (c *Client) SetPriceCallback(callback func(*exchanges.PriceUpdate)) {
	c.ws.SetCallback(callback)
}

// SetErrorCallback устанавливает callback для критических ошибок WebSocket.
//
// Вызывается при невозможности восстановить соединение после всех попыток.
// Используйте для перевода зависимых систем в режим паузы.
func (c *Client) SetErrorCallback(callback func(error)) {
	c.ws.SetErrorCallback(callback)
}

// GetSubscriptions возвращает список активных подписок.
func (c *Client) GetSubscriptions() []string {
	return c.ws.GetSubscriptions()
}

// IsWebSocketConnected возвращает статус Public WebSocket соединения.
func (c *Client) IsWebSocketConnected() bool {
	return c.ws.IsConnected()
}

// =============================================================================
// Методы Private WebSocket (мониторинг позиций и ликвидаций)
// =============================================================================

// SetPositionCallback устанавливает callback для обновлений позиций.
//
// Callback вызывается при любом изменении позиции, включая ликвидации.
// Согласно TZ.md 2.5C: "Ликвидация: Если биржа ликвидировала одну позицию →
// Немедленно закрыть вторую позицию"
//
// Пример использования для мониторинга ликвидаций:
//
//	client.SetPositionCallback(func(event *bybit.PositionEvent) {
//	    if event.IsLiquidated {
//	        log.Printf("LIQUIDATION: %s %s", event.Symbol, event.Side)
//	        // Закрываем противоположную позицию на другой бирже
//	        arbitrage.HandleLiquidation(event.Symbol)
//	    }
//	})
//
// Потокобезопасен — можно вызывать из любой горутины.
func (c *Client) SetPositionCallback(callback func(*PositionEvent)) {
	if c.privateWs != nil {
		c.privateWs.SetPositionCallback(callback)
	}
}

// SetExecutionCallback устанавливает callback для исполнений ордеров.
//
// Callback вызывается при каждом исполнении ордера (частичном или полном).
// Может использоваться для отслеживания исполнений и ликвидаций.
//
// Пример:
//
//	client.SetExecutionCallback(func(event *bybit.ExecutionEvent) {
//	    if event.IsLiquidation {
//	        log.Printf("LIQUIDATION EXECUTION: %s", event.OrderID)
//	    }
//	})
//
// Потокобезопасен — можно вызывать из любой горутины.
func (c *Client) SetExecutionCallback(callback func(*ExecutionEvent)) {
	if c.privateWs != nil {
		c.privateWs.SetExecutionCallback(callback)
	}
}

// SetPrivateErrorCallback устанавливает callback для ошибок Private WebSocket.
//
// Вызывается при невозможности восстановить приватное соединение.
// Рекомендуется логировать и уведомлять о проблемах с мониторингом ликвидаций.
func (c *Client) SetPrivateErrorCallback(callback func(error)) {
	if c.privateWs != nil {
		c.privateWs.SetErrorCallback(callback)
	}
}

// IsPrivateWebSocketConnected возвращает статус Private WebSocket соединения.
//
// Если false — мониторинг ликвидаций недоступен.
func (c *Client) IsPrivateWebSocketConnected() bool {
	if c.privateWs == nil {
		return false
	}
	return c.privateWs.IsConnected()
}

// GetPrivateWebSocketClient возвращает Private WebSocket клиент для прямого использования.
// Используйте с осторожностью — предпочитайте методы Client.
func (c *Client) GetPrivateWebSocketClient() *PrivateWebSocketClient {
	return c.privateWs
}

// =============================================================================
// Статус и диагностика
// =============================================================================

// IsConnected возвращает общий статус подключения.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected && c.ws.IsConnected()
}

// Ping проверяет доступность API.
func (c *Client) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), QueryOperationTimeout)
	defer cancel()

	return c.rest.Ping(ctx)
}

// GetRestClient возвращает REST клиент для прямого использования.
// Используйте с осторожностью — предпочитайте методы Client.
func (c *Client) GetRestClient() *RestClient {
	return c.rest
}

// GetWebSocketClient возвращает WebSocket клиент для прямого использования.
// Используйте с осторожностью — предпочитайте методы Client.
func (c *Client) GetWebSocketClient() *WebSocketClient {
	return c.ws
}

// SetLogger устанавливает логгер для всех компонентов.
// Потокобезопасен — можно вызывать из любой горутины.
func (c *Client) SetLogger(logger *zap.Logger) {
	if logger != nil {
		c.logger.Store(logger)
		c.rest.SetLogger(logger)
		c.ws.SetLogger(logger)
		if c.privateWs != nil {
			c.privateWs.SetLogger(logger)
		}
	}
}

// =============================================================================
// Проверка реализации интерфейса
// =============================================================================

// Compile-time проверка, что Client реализует Exchange interface.
var _ exchanges.Exchange = (*Client)(nil)
