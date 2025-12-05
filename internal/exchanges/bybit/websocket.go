package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"arbitrage-terminal/internal/exchanges"
	"arbitrage-terminal/pkg/metrics"

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
//
// Потокобезопасность:
//   - callback защищён через atomic.Value
//   - errorCallback защищён через atomic.Value
//   - logger защищён через atomic.Value
//   - subscriptions защищены через sync.RWMutex
//   - conn защищён через sync.RWMutex
type WebSocketClient struct {
	url           string          // URL WebSocket сервера
	conn          *websocket.Conn // Активное соединение
	subscriptions map[string]bool // Активные подписки (topic -> subscribed)

	// Каналы управления
	closeCh    chan struct{} // Сигнал закрытия
	stopLoopCh chan struct{} // Сигнал остановки текущих горутин при reconnect

	// Callbacks и logger (atomic для потокобезопасности)
	callback      atomic.Value // func(*exchanges.PriceUpdate)
	errorCallback atomic.Value // func(error)
	logger        atomic.Value // *zap.Logger

	// Состояние
	connected  atomic.Bool // Флаг подключения
	shouldStop atomic.Bool // Флаг остановки

	// Синхронизация
	mu      sync.RWMutex   // Защита subscriptions и conn
	writeMu sync.Mutex     // Защита записи в WebSocket
	wg      sync.WaitGroup // Ожидание завершения горутин
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

	ws := &WebSocketClient{
		url:           url,
		subscriptions: make(map[string]bool),
		closeCh:       make(chan struct{}),
		stopLoopCh:    make(chan struct{}),
	}

	// Устанавливаем callbacks и logger через atomic
	ws.logger.Store(logger)
	ws.callback.Store(cfg.Callback)
	if cfg.ErrorCallback != nil {
		ws.errorCallback.Store(cfg.ErrorCallback)
	}

	return ws, nil
}

// getLogger возвращает текущий логгер (потокобезопасно).
func (ws *WebSocketClient) getLogger() *zap.Logger {
	if l := ws.logger.Load(); l != nil {
		return l.(*zap.Logger)
	}
	return zap.NewNop()
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
		ReadBufferSize:   ReadBufferSize,
		WriteBufferSize:  WriteBufferSize,
		HandshakeTimeout: 10 * time.Second,
	}

	// Подключение
	conn, _, err := dialer.Dial(ws.url, nil)
	if err != nil {
		ws.getLogger().Error("failed to connect to WebSocket",
			zap.String("url", ws.url),
			zap.Error(err),
		)
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	ws.conn = conn
	ws.connected.Store(true)
	ws.shouldStop.Store(false)

	// Пересоздаём канал остановки для новых горутин
	ws.stopLoopCh = make(chan struct{})

	ws.getLogger().Info("WebSocket connected",
		zap.String("url", ws.url),
	)

	// Обновляем метрику подключения
	metrics.SetWebSocketConnected(ExchangeName, true)

	// Запуск горутин обработки
	ws.wg.Add(2)
	go ws.runMessageLoop()
	go ws.runPingLoop()

	return nil
}

// Close закрывает WebSocket соединение.
func (ws *WebSocketClient) Close() error {
	// Проверяем, не закрыты ли мы уже
	if ws.shouldStop.Load() {
		return nil
	}
	ws.shouldStop.Store(true)

	// Сигнал закрытия всем горутинам
	close(ws.closeCh)

	// Закрываем соединение
	ws.mu.Lock()
	if ws.conn != nil {
		ws.conn.Close()
		ws.conn = nil
	}
	ws.connected.Store(false)
	ws.mu.Unlock()

	// Обновляем метрику подключения
	metrics.SetWebSocketConnected(ExchangeName, false)

	// Ждём завершения горутин с таймаутом
	done := make(chan struct{})
	go func() {
		ws.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Горутины завершились нормально
	case <-time.After(5 * time.Second):
		ws.getLogger().Warn("timeout waiting for goroutines to stop")
	}

	ws.getLogger().Info("WebSocket closed")
	return nil
}

// IsConnected возвращает статус подключения.
func (ws *WebSocketClient) IsConnected() bool {
	return ws.connected.Load()
}

// =============================================================================
// Горутины обработки с recover
// =============================================================================

// runMessageLoop читает и обрабатывает входящие сообщения.
// Горутина защищена от паники через recover.
func (ws *WebSocketClient) runMessageLoop() {
	defer ws.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			ws.getLogger().Error("panic in message loop",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
			// Инициируем reconnect при панике
			ws.handleConnectionError(fmt.Errorf("panic: %v", r))
		}
	}()

	for {
		select {
		case <-ws.closeCh:
			return
		case <-ws.stopLoopCh:
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

		// Устанавливаем deadline для чтения
		conn.SetReadDeadline(time.Now().Add(PingInterval + PongTimeout))

		// Читаем сообщение
		_, message, err := conn.ReadMessage()
		if err != nil {
			if ws.shouldStop.Load() {
				return // Нормальное закрытие
			}

			ws.getLogger().Error("WebSocket read error",
				zap.Error(err),
			)

			ws.handleConnectionError(err)
			return
		}

		// Обрабатываем сообщение
		ws.processMessage(message)
	}
}

// runPingLoop отправляет ping сообщения для поддержания соединения.
// Горутина защищена от паники через recover.
func (ws *WebSocketClient) runPingLoop() {
	defer ws.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			ws.getLogger().Error("panic in ping loop",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
		}
	}()

	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ws.closeCh:
			return
		case <-ws.stopLoopCh:
			return
		case <-ticker.C:
			if !ws.connected.Load() {
				continue
			}

			if err := ws.sendPing(); err != nil {
				ws.getLogger().Warn("ping failed",
					zap.Error(err),
				)
				// Не инициируем reconnect на ошибке ping,
				// read error сделает это
			}
		}
	}
}

// handleConnectionError обрабатывает ошибку соединения и запускает reconnect.
func (ws *WebSocketClient) handleConnectionError(err error) {
	ws.connected.Store(false)
	metrics.SetWebSocketConnected(ExchangeName, false)

	// Останавливаем текущие горутины
	select {
	case <-ws.stopLoopCh:
		// Уже закрыт
	default:
		close(ws.stopLoopCh)
	}

	// Запускаем reconnect в отдельной горутине
	go ws.reconnect()
}

// =============================================================================
// Обработка сообщений (hot path)
// =============================================================================

// processMessage обрабатывает одно входящее сообщение.
// Оптимизировано для минимальной латентности в hot path.
func (ws *WebSocketClient) processMessage(data []byte) {
	// Запускаем таймер для метрики парсинга
	parseTimer := metrics.NewTimer()

	// Определяем тип сообщения
	msgType := DetectMessageType(data)

	switch msgType {
	case MessageTypeOrderbook:
		// Используем пул для PriceUpdate
		update, err := ParseOrderbookMessagePooled(data)
		if err != nil {
			ws.getLogger().Warn("failed to parse orderbook",
				zap.Error(err),
			)
			return
		}

		// Записываем метрику парсинга
		metrics.JSONParsingDuration.WithLabelValues(ExchangeName, "orderbook").Observe(parseTimer.ElapsedMs())

		// Валидация
		if err := ValidatePriceUpdate(update); err != nil {
			// Возвращаем в пул при ошибке валидации
			exchanges.PutPriceUpdateWithOrderBook(update)
			return
		}

		// Метрика события
		metrics.IncrementWebSocketEvent(ExchangeName, update.Symbol, "orderbook")

		// Вызываем callback (потокобезопасно через atomic)
		if cb := ws.callback.Load(); cb != nil {
			cb.(func(*exchanges.PriceUpdate))(update)
			// ВАЖНО: callback отвечает за возврат в пул после обработки
		} else {
			// Если callback не установлен, возвращаем объект в пул сразу
			exchanges.PutPriceUpdateWithOrderBook(update)
		}

	case MessageTypeTicker:
		update, err := ParseTickerMessagePooled(data)
		if err != nil {
			ws.getLogger().Warn("failed to parse ticker",
				zap.Error(err),
			)
			return
		}

		metrics.JSONParsingDuration.WithLabelValues(ExchangeName, "ticker").Observe(parseTimer.ElapsedMs())
		metrics.IncrementWebSocketEvent(ExchangeName, update.Symbol, "ticker")

		if cb := ws.callback.Load(); cb != nil {
			cb.(func(*exchanges.PriceUpdate))(update)
			// ВАЖНО: callback отвечает за возврат в пул после обработки
		} else {
			// Если callback не установлен, возвращаем объект в пул сразу
			exchanges.PutPriceUpdate(update)
		}

	case MessageTypePong:
		metrics.IncrementWebSocketEvent(ExchangeName, "", "pong")

	case MessageTypeSubscribe:
		// Логируем только при debug уровне
		logger := ws.getLogger()
		if logger.Core().Enabled(zap.DebugLevel) {
			logger.Debug("subscription confirmed")
		}

	default:
		// Неизвестные сообщения игнорируем без логирования в hot path
	}
}

// =============================================================================
// Ping
// =============================================================================

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

// reconnect выполняет переподключение с exponential backoff.
// Защищено от паники через recover.
func (ws *WebSocketClient) reconnect() {
	defer func() {
		if r := recover(); r != nil {
			ws.getLogger().Error("panic in reconnect",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
		}
	}()

	backoff := InitialReconnectDelay

	for attempt := 1; attempt <= MaxReconnectAttempts; attempt++ {
		if ws.shouldStop.Load() {
			return
		}

		ws.getLogger().Info("reconnect attempt",
			zap.Int("attempt", attempt),
			zap.Duration("backoff", backoff),
		)

		// Ждём перед попыткой (с возможностью прерывания)
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
			ReadBufferSize:   ReadBufferSize,
			WriteBufferSize:  WriteBufferSize,
			HandshakeTimeout: 10 * time.Second,
		}

		conn, _, err := dialer.Dial(ws.url, nil)
		if err != nil {
			ws.getLogger().Error("reconnect failed",
				zap.Int("attempt", attempt),
				zap.Error(err),
			)

			metrics.IncrementReconnect(ExchangeName, false)

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
		ws.stopLoopCh = make(chan struct{}) // Новый канал для новых горутин
		ws.connected.Store(true)
		ws.mu.Unlock()

		ws.getLogger().Info("reconnected successfully",
			zap.Int("attempt", attempt),
		)

		metrics.IncrementReconnect(ExchangeName, true)
		metrics.SetWebSocketConnected(ExchangeName, true)

		// Восстанавливаем подписки
		ws.resubscribe()

		// Перезапускаем горутины обработки
		ws.wg.Add(2)
		go ws.runMessageLoop()
		go ws.runPingLoop()

		return
	}

	// Все попытки неудачны
	ws.getLogger().Error("failed to reconnect after max attempts",
		zap.Int("maxAttempts", MaxReconnectAttempts),
	)

	// Уведомляем об ошибке через callback
	if cb := ws.errorCallback.Load(); cb != nil {
		cb.(func(error))(fmt.Errorf("websocket reconnect failed after %d attempts", MaxReconnectAttempts))
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

	ws.getLogger().Info("resubscribing to topics",
		zap.Int("count", len(topics)),
	)

	// Подписываемся заново
	if err := ws.subscribe(topics...); err != nil {
		ws.getLogger().Error("resubscribe failed",
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

	ws.getLogger().Info("subscribed to topics",
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

	ws.getLogger().Info("unsubscribed from topics",
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
// Потокобезопасен.
func (ws *WebSocketClient) SetCallback(callback func(*exchanges.PriceUpdate)) {
	if callback != nil {
		ws.callback.Store(callback)
	}
}

// SetErrorCallback устанавливает callback для критических ошибок.
// Потокобезопасен.
func (ws *WebSocketClient) SetErrorCallback(callback func(error)) {
	if callback != nil {
		ws.errorCallback.Store(callback)
	}
}

// SetLogger устанавливает логгер.
// Потокобезопасен — можно вызывать из любой горутины.
func (ws *WebSocketClient) SetLogger(logger *zap.Logger) {
	if logger != nil {
		ws.logger.Store(logger)
	}
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
