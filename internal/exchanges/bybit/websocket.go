package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"arbitrage-terminal/internal/exchanges"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// =============================================================================
// Константы WebSocket
// =============================================================================

const (
	// PingInterval — интервал отправки ping сообщений.
	// Bybit требует ping каждые 20 секунд для поддержания соединения.
	PingInterval = 20 * time.Second

	// PongTimeout — таймаут ожидания pong ответа.
	PongTimeout = 10 * time.Second

	// WriteTimeout — таймаут записи в WebSocket.
	WriteTimeout = 5 * time.Second

	// ReadBufferSize — размер буфера чтения.
	ReadBufferSize = 4096

	// WriteBufferSize — размер буфера записи.
	WriteBufferSize = 4096

	// MaxReconnectAttempts — максимальное количество попыток переподключения.
	MaxReconnectAttempts = 5

	// InitialReconnectDelay — начальная задержка перед reconnect.
	InitialReconnectDelay = 2 * time.Second

	// MaxReconnectDelay — максимальная задержка перед reconnect.
	MaxReconnectDelay = 32 * time.Second

	// OrderbookDepth — глубина стакана для подписки.
	OrderbookDepth = 50
)

// =============================================================================
// WebSocketClient — клиент WebSocket для Bybit
// =============================================================================

// WebSocketClient реализует WebSocket соединение с Bybit.
// Поддерживает автоматический reconnect, ping/pong и управление подписками.
type WebSocketClient struct {
	url           string                              // URL WebSocket сервера
	conn          *websocket.Conn                     // Активное соединение
	subscriptions map[string]bool                     // Активные подписки (topic -> subscribed)
	callback      func(*exchanges.PriceUpdate)        // Callback для обновлений цен
	errorCallback func(error)                         // Callback для критических ошибок
	logger        *zap.Logger                         // Логгер

	// Каналы управления
	closeCh     chan struct{} // Сигнал закрытия
	reconnectCh chan struct{} // Сигнал переподключения
	writeCh     chan []byte   // Канал для записи сообщений

	// Состояние
	connected  atomic.Bool // Флаг подключения
	shouldStop atomic.Bool // Флаг остановки

	// Синхронизация
	mu       sync.RWMutex // Защита subscriptions и conn
	writeMu  sync.Mutex   // Защита записи в WebSocket
	wg       sync.WaitGroup // Ожидание завершения горутин
}

// WebSocketConfig содержит конфигурацию WebSocket клиента.
type WebSocketConfig struct {
	URL           string                       // URL (по умолчанию WsPublicURL)
	Callback      func(*exchanges.PriceUpdate) // Callback для обновлений (обязательный)
	ErrorCallback func(error)                  // Callback для ошибок (опционально)
	Logger        *zap.Logger                  // Логгер (опционально)
}

// NewWebSocketClient создаёт новый WebSocket клиент.
func NewWebSocketClient(cfg WebSocketConfig) (*WebSocketClient, error) {
	if cfg.Callback == nil {
		return nil, fmt.Errorf("callback is required")
	}

	url := cfg.URL
	if url == "" {
		url = WsPublicURL
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	return &WebSocketClient{
		url:           url,
		subscriptions: make(map[string]bool),
		callback:      cfg.Callback,
		errorCallback: cfg.ErrorCallback,
		logger:        logger,
		closeCh:       make(chan struct{}),
		reconnectCh:   make(chan struct{}, 1),
		writeCh:       make(chan []byte, 100),
	}, nil
}

// =============================================================================
// Подключение и отключение
// =============================================================================

// Connect устанавливает WebSocket соединение.
func (ws *WebSocketClient) Connect() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.connected.Load() {
		return nil // Уже подключены
	}

	// Настройка dialer
	dialer := websocket.Dialer{
		ReadBufferSize:  ReadBufferSize,
		WriteBufferSize: WriteBufferSize,
		HandshakeTimeout: 10 * time.Second,
	}

	// Подключение
	conn, _, err := dialer.Dial(ws.url, nil)
	if err != nil {
		ws.logger.Error("failed to connect to WebSocket",
			zap.String("url", ws.url),
			zap.Error(err),
		)
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	ws.conn = conn
	ws.connected.Store(true)
	ws.shouldStop.Store(false)

	ws.logger.Info("WebSocket connected",
		zap.String("url", ws.url),
	)

	// Запуск горутин обработки
	ws.wg.Add(3)
	go ws.handleMessages()
	go ws.handlePing()
	go ws.handleReconnect()

	return nil
}

// Close закрывает WebSocket соединение.
func (ws *WebSocketClient) Close() error {
	ws.shouldStop.Store(true)

	// Сигнал закрытия
	close(ws.closeCh)

	// Закрываем соединение
	ws.mu.Lock()
	if ws.conn != nil {
		ws.conn.Close()
		ws.conn = nil
	}
	ws.connected.Store(false)
	ws.mu.Unlock()

	// Ждём завершения горутин
	ws.wg.Wait()

	ws.logger.Info("WebSocket closed")
	return nil
}

// IsConnected возвращает статус подключения.
func (ws *WebSocketClient) IsConnected() bool {
	return ws.connected.Load()
}

// =============================================================================
// Обработка сообщений
// =============================================================================

// handleMessages читает и обрабатывает входящие сообщения.
func (ws *WebSocketClient) handleMessages() {
	defer ws.wg.Done()

	for {
		// Проверка на закрытие
		select {
		case <-ws.closeCh:
			return
		default:
		}

		// Получаем соединение
		ws.mu.RLock()
		conn := ws.conn
		ws.mu.RUnlock()

		if conn == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Читаем сообщение
		_, message, err := conn.ReadMessage()
		if err != nil {
			if ws.shouldStop.Load() {
				return // Нормальное закрытие
			}

			ws.logger.Error("WebSocket read error",
				zap.Error(err),
			)

			ws.connected.Store(false)
			ws.triggerReconnect()
			return
		}

		// Обрабатываем сообщение
		ws.processMessage(message)
	}
}

// processMessage обрабатывает одно входящее сообщение.
func (ws *WebSocketClient) processMessage(data []byte) {
	// Определяем тип сообщения
	msgType := DetectMessageType(data)

	switch msgType {
	case MessageTypeOrderbook:
		update, err := ParseOrderbookMessage(data)
		if err != nil {
			ws.logger.Warn("failed to parse orderbook",
				zap.Error(err),
			)
			return
		}

		// Валидация
		if err := ValidatePriceUpdate(update); err != nil {
			ws.logger.Debug("invalid price update",
				zap.Error(err),
			)
			return
		}

		// Callback
		if ws.callback != nil {
			ws.callback(update)
		}

	case MessageTypeTicker:
		update, err := ParseTickerMessage(data)
		if err != nil {
			ws.logger.Warn("failed to parse ticker",
				zap.Error(err),
			)
			return
		}

		if ws.callback != nil {
			ws.callback(update)
		}

	case MessageTypePong:
		ws.logger.Debug("pong received")

	case MessageTypeSubscribe:
		ws.logger.Debug("subscription confirmed")

	default:
		// Игнорируем неизвестные сообщения
		ws.logger.Debug("unknown message type",
			zap.ByteString("data", data[:min(len(data), 200)]),
		)
	}
}

// =============================================================================
// Ping/Pong
// =============================================================================

// handlePing отправляет ping сообщения для поддержания соединения.
func (ws *WebSocketClient) handlePing() {
	defer ws.wg.Done()

	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ws.closeCh:
			return

		case <-ticker.C:
			if !ws.connected.Load() {
				continue
			}

			if err := ws.sendPing(); err != nil {
				ws.logger.Warn("ping failed",
					zap.Error(err),
				)
				ws.triggerReconnect()
				return
			}
		}
	}
}

// sendPing отправляет ping сообщение.
func (ws *WebSocketClient) sendPing() error {
	pingMsg := WsPingRequest{
		Op: "ping",
	}

	data, err := json.Marshal(pingMsg)
	if err != nil {
		return err
	}

	return ws.writeMessage(data)
}

// =============================================================================
// Reconnect
// =============================================================================

// handleReconnect обрабатывает переподключение.
func (ws *WebSocketClient) handleReconnect() {
	defer ws.wg.Done()

	for {
		select {
		case <-ws.closeCh:
			return

		case <-ws.reconnectCh:
			if ws.shouldStop.Load() {
				return
			}

			ws.logger.Info("starting reconnect sequence")
			ws.reconnect()
		}
	}
}

// triggerReconnect инициирует переподключение.
func (ws *WebSocketClient) triggerReconnect() {
	select {
	case ws.reconnectCh <- struct{}{}:
	default:
		// Reconnect уже запланирован
	}
}

// reconnect выполняет переподключение с exponential backoff.
func (ws *WebSocketClient) reconnect() {
	backoff := InitialReconnectDelay

	for attempt := 1; attempt <= MaxReconnectAttempts; attempt++ {
		if ws.shouldStop.Load() {
			return
		}

		ws.logger.Info("reconnect attempt",
			zap.Int("attempt", attempt),
			zap.Duration("backoff", backoff),
		)

		// Ждём перед попыткой
		select {
		case <-ws.closeCh:
			return
		case <-time.After(backoff):
		}

		// Закрываем старое соединение
		ws.mu.Lock()
		if ws.conn != nil {
			ws.conn.Close()
			ws.conn = nil
		}
		ws.mu.Unlock()

		// Пытаемся подключиться
		dialer := websocket.Dialer{
			ReadBufferSize:  ReadBufferSize,
			WriteBufferSize: WriteBufferSize,
			HandshakeTimeout: 10 * time.Second,
		}

		conn, _, err := dialer.Dial(ws.url, nil)
		if err != nil {
			ws.logger.Error("reconnect failed",
				zap.Int("attempt", attempt),
				zap.Error(err),
			)

			// Exponential backoff
			backoff *= 2
			if backoff > MaxReconnectDelay {
				backoff = MaxReconnectDelay
			}
			continue
		}

		// Успешное подключение
		ws.mu.Lock()
		ws.conn = conn
		ws.connected.Store(true)
		ws.mu.Unlock()

		ws.logger.Info("reconnected successfully",
			zap.Int("attempt", attempt),
		)

		// Восстанавливаем подписки
		ws.resubscribe()

		// Перезапускаем обработчик сообщений
		ws.wg.Add(1)
		go ws.handleMessages()

		return
	}

	// Все попытки неудачны
	ws.logger.Error("failed to reconnect after max attempts",
		zap.Int("maxAttempts", MaxReconnectAttempts),
	)

	// Уведомляем об ошибке
	if ws.errorCallback != nil {
		ws.errorCallback(fmt.Errorf("websocket reconnect failed after %d attempts", MaxReconnectAttempts))
	}
}

// resubscribe восстанавливает все подписки после reconnect.
func (ws *WebSocketClient) resubscribe() {
	ws.mu.RLock()
	topics := make([]string, 0, len(ws.subscriptions))
	for topic := range ws.subscriptions {
		topics = append(topics, topic)
	}
	ws.mu.RUnlock()

	if len(topics) == 0 {
		return
	}

	ws.logger.Info("resubscribing to topics",
		zap.Int("count", len(topics)),
	)

	// Подписываемся заново
	if err := ws.subscribe(topics...); err != nil {
		ws.logger.Error("resubscribe failed",
			zap.Error(err),
		)
	}
}

// =============================================================================
// Подписки
// =============================================================================

// Subscribe подписывается на обновления стакана для указанных символов.
func (ws *WebSocketClient) Subscribe(symbols ...string) error {
	if len(symbols) == 0 {
		return nil
	}

	// Формируем топики для подписки
	topics := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		topic := fmt.Sprintf("orderbook.%d.%s", OrderbookDepth, symbol)
		topics = append(topics, topic)
	}

	return ws.subscribe(topics...)
}

// SubscribeTickers подписывается на тикеры для указанных символов.
func (ws *WebSocketClient) SubscribeTickers(symbols ...string) error {
	if len(symbols) == 0 {
		return nil
	}

	topics := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		topic := fmt.Sprintf("tickers.%s", symbol)
		topics = append(topics, topic)
	}

	return ws.subscribe(topics...)
}

// subscribe выполняет подписку на указанные топики.
func (ws *WebSocketClient) subscribe(topics ...string) error {
	if len(topics) == 0 {
		return nil
	}

	// Формируем запрос подписки
	req := WsSubscribeRequest{
		Op:   "subscribe",
		Args: topics,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal subscribe request: %w", err)
	}

	// Отправляем
	if err := ws.writeMessage(data); err != nil {
		return fmt.Errorf("failed to send subscribe request: %w", err)
	}

	// Сохраняем подписки
	ws.mu.Lock()
	for _, topic := range topics {
		ws.subscriptions[topic] = true
	}
	ws.mu.Unlock()

	ws.logger.Info("subscribed to topics",
		zap.Strings("topics", topics),
	)

	return nil
}

// Unsubscribe отписывается от обновлений стакана для указанных символов.
func (ws *WebSocketClient) Unsubscribe(symbols ...string) error {
	if len(symbols) == 0 {
		return nil
	}

	topics := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		topic := fmt.Sprintf("orderbook.%d.%s", OrderbookDepth, symbol)
		topics = append(topics, topic)
	}

	return ws.unsubscribe(topics...)
}

// unsubscribe выполняет отписку от указанных топиков.
func (ws *WebSocketClient) unsubscribe(topics ...string) error {
	if len(topics) == 0 {
		return nil
	}

	req := WsUnsubscribeRequest{
		Op:   "unsubscribe",
		Args: topics,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal unsubscribe request: %w", err)
	}

	if err := ws.writeMessage(data); err != nil {
		return fmt.Errorf("failed to send unsubscribe request: %w", err)
	}

	// Удаляем подписки
	ws.mu.Lock()
	for _, topic := range topics {
		delete(ws.subscriptions, topic)
	}
	ws.mu.Unlock()

	ws.logger.Info("unsubscribed from topics",
		zap.Strings("topics", topics),
	)

	return nil
}

// GetSubscriptions возвращает список активных подписок.
func (ws *WebSocketClient) GetSubscriptions() []string {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	topics := make([]string, 0, len(ws.subscriptions))
	for topic := range ws.subscriptions {
		topics = append(topics, topic)
	}
	return topics
}

// =============================================================================
// Вспомогательные методы
// =============================================================================

// writeMessage записывает сообщение в WebSocket с синхронизацией.
func (ws *WebSocketClient) writeMessage(data []byte) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.RLock()
	conn := ws.conn
	ws.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	// Устанавливаем таймаут записи
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return err
	}

	return conn.WriteMessage(websocket.TextMessage, data)
}

// SetCallback устанавливает callback для обновлений цен.
func (ws *WebSocketClient) SetCallback(callback func(*exchanges.PriceUpdate)) {
	ws.callback = callback
}

// SetErrorCallback устанавливает callback для критических ошибок.
func (ws *WebSocketClient) SetErrorCallback(callback func(error)) {
	ws.errorCallback = callback
}

// SetLogger устанавливает логгер.
func (ws *WebSocketClient) SetLogger(logger *zap.Logger) {
	ws.logger = logger
}

// =============================================================================
// Контекстное управление
// =============================================================================

// ConnectWithContext устанавливает соединение с поддержкой контекста.
func (ws *WebSocketClient) ConnectWithContext(ctx context.Context) error {
	// Запускаем подключение
	errCh := make(chan error, 1)
	go func() {
		errCh <- ws.Connect()
	}()

	select {
	case <-ctx.Done():
		ws.Close()
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// SubscribeWithContext подписывается с поддержкой контекста.
func (ws *WebSocketClient) SubscribeWithContext(ctx context.Context, symbols ...string) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- ws.Subscribe(symbols...)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
