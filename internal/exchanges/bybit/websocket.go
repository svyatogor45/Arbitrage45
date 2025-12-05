package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strconv"
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
	// Согласно Requirements.md: "Для расчёта средней цены исполнения учитываем 10 уровней стакана"
	OrderbookDepth = 10

	// GracefulShutdownTimeout — таймаут ожидания завершения горутин при закрытии.
	GracefulShutdownTimeout = 5 * time.Second
)

// =============================================================================
// WebSocketClient — клиент WebSocket для Bybit
// =============================================================================

// WebSocketClient реализует WebSocket соединение с Bybit.
// Поддерживает автоматический reconnect, ping/pong и управление подписками.
//
// Потокобезопасность:
//   - Все поля защищены соответствующими примитивами синхронизации
//   - callback, errorCallback, logger — через atomic.Value
//   - subscriptions, conn, stopLoopCh — через sync.RWMutex (mu)
//   - Запись в WebSocket — через sync.Mutex (writeMu)
//
// Жизненный цикл горутин:
//   - runMessageLoop и runPingLoop запускаются при Connect/reconnect
//   - Завершаются при получении сигнала из closeCh или stopLoopCh
//   - WaitGroup отслеживает все активные горутины
type WebSocketClient struct {
	url           string          // URL WebSocket сервера
	conn          *websocket.Conn // Активное соединение (защищено mu)
	subscriptions map[string]bool // Активные подписки topic -> subscribed (защищено mu)
	stopLoopCh    chan struct{}   // Сигнал остановки текущих горутин (защищено mu)

	// Канал закрытия (никогда не пересоздаётся после создания клиента)
	closeCh chan struct{}

	// Callbacks и logger (atomic для потокобезопасности без блокировок)
	callback      atomic.Value // func(*exchanges.PriceUpdate)
	errorCallback atomic.Value // func(error)
	logger        atomic.Value // *zap.Logger

	// Состояние (atomic для lock-free доступа)
	connected     atomic.Bool   // Флаг подключения
	shouldStop    atomic.Bool   // Флаг остановки (true после вызова Close)
	reconnecting  atomic.Bool   // Флаг активного reconnect (предотвращает дублирование)
	reqCounter    atomic.Uint64 // Счётчик запросов для генерации ReqId

	// Синхронизация
	mu      sync.RWMutex   // Защита conn, subscriptions, stopLoopCh
	writeMu sync.Mutex     // Защита записи в WebSocket (отдельный мьютекс для производительности)
	wg      sync.WaitGroup // Ожидание завершения горутин
}

// nextReqID генерирует уникальный ReqId для запросов.
// Формат: "bybit_{counter}" для трассировки в логах.
func (ws *WebSocketClient) nextReqID() string {
	id := ws.reqCounter.Add(1)
	return "bybit_" + strconv.FormatUint(id, 10)
}

// WebSocketConfig содержит конфигурацию WebSocket клиента.
type WebSocketConfig struct {
	URL           string                       // URL (по умолчанию WsPublicURL)
	Callback      func(*exchanges.PriceUpdate) // Callback для обновлений (обязательный)
	ErrorCallback func(error)                  // Callback для ошибок (опционально)
	Logger        *zap.Logger                  // Логгер (опционально)
}

// NewWebSocketClient создаёт новый WebSocket клиент.
//
// Параметры:
//   - cfg: конфигурация клиента (Callback обязателен)
//
// Возвращает ошибку, если Callback не указан.
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
//
// Метод идемпотентен — повторный вызов при активном соединении возвращает nil.
// При успешном подключении запускаются горутины обработки сообщений и ping.
func (ws *WebSocketClient) Connect() error {
	// Проверяем, не закрыт ли клиент
	if ws.shouldStop.Load() {
		return fmt.Errorf("websocket client is closed")
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.connected.Load() {
		return nil // Уже подключены
	}

	// Настройка dialer с оптимизированными буферами
	dialer := websocket.Dialer{
		ReadBufferSize:   ReadBufferSize,
		WriteBufferSize:  WriteBufferSize,
		HandshakeTimeout: 10 * time.Second,
	}

	// Подключение
	conn, _, err := dialer.Dial(ws.url, nil)
	if err != nil {
		ws.getLogger().Error("не удалось подключиться к WebSocket",
			zap.String("url", ws.url),
			zap.Error(err),
		)
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	// Настраиваем pong handler для WebSocket ping frames
	conn.SetPongHandler(func(appData string) error {
		metrics.IncrementWebSocketEvent(ExchangeName, "", "pong")
		return nil
	})

	ws.conn = conn
	ws.connected.Store(true)

	// Пересоздаём канал остановки для новых горутин
	ws.stopLoopCh = make(chan struct{})

	ws.getLogger().Info("WebSocket подключён",
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

// Close закрывает WebSocket соединение и освобождает ресурсы.
//
// Метод идемпотентен — повторный вызов безопасен.
// Блокируется до завершения всех горутин или до истечения таймаута.
func (ws *WebSocketClient) Close() error {
	// Атомарная проверка и установка флага (предотвращает повторное закрытие)
	if ws.shouldStop.Swap(true) {
		return nil // Уже закрываемся
	}

	// Сигнал закрытия всем горутинам (closeCh никогда не пересоздаётся)
	close(ws.closeCh)

	// Закрываем соединение под мьютексом
	ws.mu.Lock()
	if ws.conn != nil {
		// Отправляем close frame для graceful shutdown
		ws.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(WriteTimeout),
		)
		ws.conn.Close()
		ws.conn = nil
	}
	ws.connected.Store(false)

	// Закрываем stopLoopCh если ещё не закрыт
	select {
	case <-ws.stopLoopCh:
		// Уже закрыт
	default:
		close(ws.stopLoopCh)
	}
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
		ws.getLogger().Debug("все горутины завершены")
	case <-time.After(GracefulShutdownTimeout):
		ws.getLogger().Warn("таймаут ожидания завершения горутин")
	}

	ws.getLogger().Info("WebSocket закрыт")
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
//
// Горутина защищена от паники через recover.
// Завершается при:
//   - Получении сигнала из closeCh (полное закрытие клиента)
//   - Получении сигнала из stopLoopCh (reconnect)
//   - Ошибке чтения из WebSocket
func (ws *WebSocketClient) runMessageLoop() {
	defer ws.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			ws.getLogger().Error("паника в цикле обработки сообщений",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
			// Инициируем reconnect при панике
			ws.handleConnectionError(fmt.Errorf("panic: %v", r))
		}
	}()

	for {
		// Проверяем сигналы завершения (под мьютексом для stopLoopCh)
		ws.mu.RLock()
		stopCh := ws.stopLoopCh
		ws.mu.RUnlock()

		select {
		case <-ws.closeCh:
			return
		case <-stopCh:
			return
		default:
		}

		// Получаем соединение под мьютексом
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
			// Проверяем, не закрываемся ли мы
			if ws.shouldStop.Load() {
				return
			}

			ws.getLogger().Error("ошибка чтения WebSocket",
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
//
// Горутина защищена от паники через recover.
// Использует WebSocket ping frame для минимальной латентности.
func (ws *WebSocketClient) runPingLoop() {
	defer ws.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			ws.getLogger().Error("паника в цикле ping",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
		}
	}()

	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	for {
		// Проверяем сигналы завершения (под мьютексом для stopLoopCh)
		ws.mu.RLock()
		stopCh := ws.stopLoopCh
		ws.mu.RUnlock()

		select {
		case <-ws.closeCh:
			return
		case <-stopCh:
			return
		case <-ticker.C:
			if !ws.connected.Load() {
				continue
			}

			if err := ws.sendPing(); err != nil {
				ws.getLogger().Warn("ping не удался",
					zap.Error(err),
				)
				// Не инициируем reconnect на ошибке ping — read error сделает это
			}
		}
	}
}

// handleConnectionError обрабатывает ошибку соединения и запускает reconnect.
//
// Потокобезопасен — все операции с разделяемыми данными под мьютексом.
// Использует atomic флаг reconnecting для предотвращения дублирования reconnect.
func (ws *WebSocketClient) handleConnectionError(err error) {
	// Проверяем, не закрываемся ли мы
	if ws.shouldStop.Load() {
		return
	}

	// Атомарная проверка — только один reconnect одновременно
	if ws.reconnecting.Swap(true) {
		return // Reconnect уже в процессе
	}

	ws.connected.Store(false)
	metrics.SetWebSocketConnected(ExchangeName, false)

	// Останавливаем текущие горутины (под мьютексом)
	ws.mu.Lock()
	select {
	case <-ws.stopLoopCh:
		// Уже закрыт
	default:
		close(ws.stopLoopCh)
	}
	ws.mu.Unlock()

	// Запускаем reconnect в отдельной горутине
	go ws.reconnect()
}

// =============================================================================
// Обработка сообщений (hot path)
// =============================================================================

// processMessage обрабатывает одно входящее сообщение.
//
// Оптимизировано для минимальной латентности:
//   - Быстрое определение типа сообщения через bytes.Contains
//   - Object pooling для PriceUpdate и OrderBook
//   - Минимум аллокаций в hot path
func (ws *WebSocketClient) processMessage(data []byte) {
	// Запускаем таймер для метрики парсинга
	parseTimer := metrics.NewTimer()

	// Определяем тип сообщения (оптимизировано)
	msgType := DetectMessageType(data)

	switch msgType {
	case MessageTypeOrderbook:
		// Используем пул для PriceUpdate
		update, err := ParseOrderbookMessagePooled(data)
		if err != nil {
			ws.getLogger().Warn("ошибка парсинга orderbook",
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
			ws.getLogger().Warn("ошибка парсинга ticker",
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
			exchanges.PutPriceUpdate(update)
		}

	case MessageTypePong:
		// JSON pong от Bybit (в дополнение к WebSocket pong frame)
		metrics.IncrementWebSocketEvent(ExchangeName, "", "pong")

	case MessageTypeSubscribe:
		// Логируем только при debug уровне (избегаем аллокаций в production)
		logger := ws.getLogger()
		if logger.Core().Enabled(zap.DebugLevel) {
			logger.Debug("подписка подтверждена")
		}

	default:
		// Неизвестные сообщения игнорируем без логирования в hot path
	}
}

// =============================================================================
// Ping
// =============================================================================

// sendPing отправляет ping для поддержания соединения.
//
// Использует WebSocket ping control frame (RFC 6455) для минимальной латентности.
// Bybit также поддерживает JSON ping, но control frame эффективнее.
func (ws *WebSocketClient) sendPing() error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.RLock()
	conn := ws.conn
	ws.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	// Используем WebSocket ping frame (более эффективно чем JSON)
	deadline := time.Now().Add(WriteTimeout)
	if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
		return fmt.Errorf("ping frame failed: %w", err)
	}

	return nil
}

// sendJSONPing отправляет JSON ping сообщение (fallback метод).
//
// Используется если биржа не поддерживает стандартные WebSocket ping frames.
// Bybit поддерживает оба варианта.
func (ws *WebSocketClient) sendJSONPing() error {
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
//
// Алгоритм:
//  1. Ждёт backoff перед попыткой (с возможностью прерывания)
//  2. Закрывает старое соединение
//  3. Создаёт новое соединение
//  4. Восстанавливает подписки
//  5. Запускает горутины обработки
//
// Защищено от паники через recover.
// Использует atomic флаг reconnecting для предотвращения дублирования.
func (ws *WebSocketClient) reconnect() {
	defer func() {
		ws.reconnecting.Store(false) // Сбрасываем флаг при выходе
		if r := recover(); r != nil {
			ws.getLogger().Error("паника в reconnect",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
		}
	}()

	backoff := InitialReconnectDelay

	for attempt := 1; attempt <= MaxReconnectAttempts; attempt++ {
		// Проверяем флаг остановки ПЕРЕД ожиданием
		if ws.shouldStop.Load() {
			return
		}

		ws.getLogger().Info("попытка переподключения",
			zap.Int("attempt", attempt),
			zap.Int("maxAttempts", MaxReconnectAttempts),
			zap.Duration("backoff", backoff),
		)

		// Ждём перед попыткой (с возможностью прерывания)
		select {
		case <-ws.closeCh:
			ws.getLogger().Debug("reconnect прерван: клиент закрывается")
			return
		case <-time.After(backoff):
		}

		// Повторная проверка после ожидания
		if ws.shouldStop.Load() {
			return
		}

		// Закрываем старое соединение под мьютексом
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
			ws.getLogger().Error("reconnect не удался",
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

		// Настраиваем pong handler
		conn.SetPongHandler(func(appData string) error {
			metrics.IncrementWebSocketEvent(ExchangeName, "", "pong")
			return nil
		})

		// Успешное подключение — обновляем состояние под мьютексом
		ws.mu.Lock()
		ws.conn = conn
		ws.stopLoopCh = make(chan struct{}) // Новый канал для новых горутин
		ws.connected.Store(true)
		ws.mu.Unlock()

		ws.getLogger().Info("переподключение успешно",
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
	ws.getLogger().Error("не удалось переподключиться после всех попыток",
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

	ws.getLogger().Info("восстановление подписок",
		zap.Int("count", len(topics)),
	)

	// Подписываемся заново
	if err := ws.subscribe(topics...); err != nil {
		ws.getLogger().Error("ошибка восстановления подписок",
			zap.Error(err),
		)
	}
}

// =============================================================================
// Подписки
// =============================================================================

// Subscribe подписывается на обновления стакана для указанных символов.
//
// Параметры:
//   - symbols: список символов (например, "BTCUSDT", "ETHUSDT")
//
// Формат топика: orderbook.{depth}.{symbol}
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
//
// Параметры:
//   - symbols: список символов
//
// Формат топика: tickers.{symbol}
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

	// Генерируем уникальный ReqId для трассировки
	reqID := ws.nextReqID()

	// Формируем запрос подписки
	req := WsSubscribeRequest{
		Op:    "subscribe",
		ReqId: reqID,
		Args:  topics,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ошибка сериализации запроса подписки: %w", err)
	}

	// Отправляем
	if err := ws.writeMessage(data); err != nil {
		return fmt.Errorf("ошибка отправки запроса подписки: %w", err)
	}

	// Сохраняем подписки
	ws.mu.Lock()
	for _, topic := range topics {
		ws.subscriptions[topic] = true
	}
	ws.mu.Unlock()

	ws.getLogger().Info("подписка на топики",
		zap.String("reqId", reqID),
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

	// Генерируем уникальный ReqId для трассировки
	reqID := ws.nextReqID()

	req := WsUnsubscribeRequest{
		Op:    "unsubscribe",
		ReqId: reqID,
		Args:  topics,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ошибка сериализации запроса отписки: %w", err)
	}

	if err := ws.writeMessage(data); err != nil {
		return fmt.Errorf("ошибка отправки запроса отписки: %w", err)
	}

	// Удаляем подписки
	ws.mu.Lock()
	for _, topic := range topics {
		delete(ws.subscriptions, topic)
	}
	ws.mu.Unlock()

	ws.getLogger().Info("отписка от топиков",
		zap.String("reqId", reqID),
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
//
// Использует отдельный мьютекс writeMu для минимизации блокировок.
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
//
// Потокобезопасен — можно вызывать из любой горутины.
// Callback получает PriceUpdate из пула и ДОЛЖЕН вернуть его после обработки.
func (ws *WebSocketClient) SetCallback(callback func(*exchanges.PriceUpdate)) {
	if callback != nil {
		ws.callback.Store(callback)
	}
}

// SetErrorCallback устанавливает callback для критических ошибок.
//
// Потокобезопасен — можно вызывать из любой горутины.
// Вызывается при невозможности восстановить соединение.
func (ws *WebSocketClient) SetErrorCallback(callback func(error)) {
	if callback != nil {
		ws.errorCallback.Store(callback)
	}
}

// SetLogger устанавливает логгер.
//
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
//
// При отмене контекста соединение закрывается и возвращается ctx.Err().
func (ws *WebSocketClient) ConnectWithContext(ctx context.Context) error {
	// Проверяем контекст перед началом
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Запускаем подключение
	errCh := make(chan error, 1)
	go func() {
		errCh <- ws.Connect()
	}()

	select {
	case <-ctx.Done():
		// Контекст отменён — закрываем соединение
		ws.Close()
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// SubscribeWithContext подписывается с поддержкой контекста.
//
// При отмене контекста возвращается ctx.Err(), подписка может быть частичной.
func (ws *WebSocketClient) SubscribeWithContext(ctx context.Context, symbols ...string) error {
	// Проверяем контекст перед началом
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

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
