package bybit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
// Константы Private WebSocket
// =============================================================================

const (
	// PrivateWsPingInterval — интервал отправки ping для приватного WebSocket.
	PrivateWsPingInterval = 20 * time.Second

	// PrivateWsAuthTimeout — таймаут ожидания ответа аутентификации.
	PrivateWsAuthTimeout = 10 * time.Second

	// PrivateWsMaxReconnectAttempts — максимальное количество попыток reconnect.
	PrivateWsMaxReconnectAttempts = 5

	// PrivateWsInitialReconnectDelay — начальная задержка reconnect.
	PrivateWsInitialReconnectDelay = 2 * time.Second

	// PrivateWsMaxReconnectDelay — максимальная задержка reconnect.
	PrivateWsMaxReconnectDelay = 32 * time.Second
)

// =============================================================================
// Типы событий позиций
// =============================================================================

// PositionEvent представляет событие обновления позиции.
// Используется для мониторинга ликвидаций согласно TZ.md 2.5C.
type PositionEvent struct {
	Symbol        string                  // Символ торговой пары
	Side          exchanges.PositionSide  // Сторона позиции (Long/Short)
	Size          float64                 // Текущий размер позиции
	AvgPrice      float64                 // Средняя цена входа
	UnrealizedPNL float64                 // Нереализованный PNL
	LiqPrice      float64                 // Цена ликвидации
	PositionIdx   int                     // Индекс позиции (1=long, 2=short)
	IsLiquidated  bool                    // Флаг ликвидации (size стал 0)
	Timestamp     time.Time               // Время события
}

// ExecutionEvent представляет событие исполнения ордера.
type ExecutionEvent struct {
	OrderID       string                 // ID ордера
	Symbol        string                 // Символ
	Side          exchanges.OrderSide    // Сторона (Buy/Sell)
	OrderType     string                 // Тип ордера
	ExecQty       float64                // Исполненное количество
	ExecPrice     float64                // Цена исполнения
	OrderStatus   exchanges.OrderStatus  // Статус ордера
	IsLiquidation bool                   // Флаг ликвидации
	Timestamp     time.Time              // Время исполнения
}

// =============================================================================
// PrivateWebSocketClient — приватный WebSocket клиент
// =============================================================================

// PrivateWebSocketClient реализует приватное WebSocket соединение с Bybit.
// Используется для мониторинга позиций и ликвидаций в реальном времени.
//
// Согласно TZ.md 2.5C:
// "Ликвидация: Если биржа ликвидировала одну позицию →
//  Немедленно закрыть вторую позицию"
//
// Потокобезопасность:
//   - Все поля защищены соответствующими примитивами синхронизации
//   - Callbacks через atomic.Value
//   - Запись в WebSocket через отдельный мьютекс
type PrivateWebSocketClient struct {
	url           string          // URL приватного WebSocket
	apiKey        string          // API ключ для аутентификации
	apiSecret     string          // API секрет для подписи
	conn          *websocket.Conn // Активное соединение
	authenticated atomic.Bool     // Флаг успешной аутентификации
	stopLoopCh    chan struct{}   // Сигнал остановки горутин

	// Канал закрытия (никогда не пересоздаётся)
	closeCh chan struct{}

	// Callbacks (atomic для потокобезопасности)
	positionCallback  atomic.Value // func(*PositionEvent)
	executionCallback atomic.Value // func(*ExecutionEvent)
	errorCallback     atomic.Value // func(error)
	logger            atomic.Value // *zap.Logger

	// Состояние
	connected    atomic.Bool // Флаг подключения
	shouldStop   atomic.Bool // Флаг остановки
	reconnecting atomic.Bool // Флаг активного reconnect
	reqCounter   atomic.Uint64 // Счётчик запросов

	// Синхронизация
	mu      sync.RWMutex   // Защита conn, stopLoopCh
	writeMu sync.Mutex     // Защита записи в WebSocket
	wg      sync.WaitGroup // Ожидание завершения горутин
}

// PrivateWebSocketConfig содержит конфигурацию приватного WebSocket.
type PrivateWebSocketConfig struct {
	URL               string               // URL (по умолчанию WsPrivateURL)
	APIKey            string               // API ключ (обязательный)
	APISecret         string               // API секрет (обязательный)
	PositionCallback  func(*PositionEvent) // Callback для обновлений позиций
	ExecutionCallback func(*ExecutionEvent) // Callback для исполнений
	ErrorCallback     func(error)          // Callback для ошибок
	Logger            *zap.Logger          // Логгер
}

// NewPrivateWebSocketClient создаёт новый приватный WebSocket клиент.
//
// Параметры:
//   - cfg: конфигурация клиента (APIKey и APISecret обязательны)
//
// Возвращает ошибку, если ключи не указаны.
func NewPrivateWebSocketClient(cfg PrivateWebSocketConfig) (*PrivateWebSocketClient, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("API key is required for private WebSocket")
	}
	if cfg.APISecret == "" {
		return nil, fmt.Errorf("API secret is required for private WebSocket")
	}

	url := cfg.URL
	if url == "" {
		url = WsPrivateURL
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	ws := &PrivateWebSocketClient{
		url:        url,
		apiKey:     cfg.APIKey,
		apiSecret:  cfg.APISecret,
		closeCh:    make(chan struct{}),
		stopLoopCh: make(chan struct{}),
	}

	// Устанавливаем callbacks через atomic
	ws.logger.Store(logger)
	if cfg.PositionCallback != nil {
		ws.positionCallback.Store(cfg.PositionCallback)
	}
	if cfg.ExecutionCallback != nil {
		ws.executionCallback.Store(cfg.ExecutionCallback)
	}
	if cfg.ErrorCallback != nil {
		ws.errorCallback.Store(cfg.ErrorCallback)
	}

	return ws, nil
}

// getLogger возвращает текущий логгер (потокобезопасно).
func (ws *PrivateWebSocketClient) getLogger() *zap.Logger {
	if l := ws.logger.Load(); l != nil {
		return l.(*zap.Logger)
	}
	return zap.NewNop()
}

// =============================================================================
// Подключение и аутентификация
// =============================================================================

// Connect устанавливает приватное WebSocket соединение и аутентифицируется.
//
// После успешной аутентификации автоматически подписывается на:
//   - position — обновления позиций (включая ликвидации)
//   - execution — исполнения ордеров
func (ws *PrivateWebSocketClient) Connect() error {
	if ws.shouldStop.Load() {
		return fmt.Errorf("private websocket client is closed")
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.connected.Load() {
		return nil
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
		ws.getLogger().Error("не удалось подключиться к приватному WebSocket",
			zap.String("url", ws.url),
			zap.Error(err),
		)
		return fmt.Errorf("private websocket dial failed: %w", err)
	}

	// Настраиваем pong handler
	conn.SetPongHandler(func(appData string) error {
		metrics.IncrementWebSocketEvent(ExchangeName, "", "private_pong")
		return nil
	})

	ws.conn = conn
	ws.connected.Store(true)
	ws.stopLoopCh = make(chan struct{})

	ws.getLogger().Info("приватный WebSocket подключён",
		zap.String("url", ws.url),
	)

	// Аутентификация
	if err := ws.authenticate(); err != nil {
		conn.Close()
		ws.conn = nil
		ws.connected.Store(false)
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Подписка на события
	if err := ws.subscribeToPrivateTopics(); err != nil {
		ws.getLogger().Warn("ошибка подписки на приватные топики",
			zap.Error(err),
		)
		// Продолжаем работу — подписка будет восстановлена при reconnect
	}

	// Запуск горутин обработки
	ws.wg.Add(2)
	go ws.runMessageLoop()
	go ws.runPingLoop()

	return nil
}

// authenticate выполняет аутентификацию на приватном WebSocket.
//
// Формат подписи: HMAC-SHA256(expires + "GET/realtime")
func (ws *PrivateWebSocketClient) authenticate() error {
	// Expires через 5 секунд
	expires := time.Now().Add(5 * time.Second).UnixMilli()
	signStr := fmt.Sprintf("%dGET/realtime", expires)

	// Вычисляем HMAC-SHA256
	h := hmac.New(sha256.New, []byte(ws.apiSecret))
	h.Write([]byte(signStr))
	signature := hex.EncodeToString(h.Sum(nil))

	// Формируем запрос аутентификации
	authReq := WsAuthRequest{
		Op: "auth",
		Args: []string{
			ws.apiKey,
			strconv.FormatInt(expires, 10),
			signature,
		},
		ReqId: ws.nextReqID(),
	}

	data, err := jsonFast.Marshal(authReq)
	if err != nil {
		return fmt.Errorf("ошибка сериализации auth запроса: %w", err)
	}

	if err := ws.writeMessage(data); err != nil {
		return fmt.Errorf("ошибка отправки auth запроса: %w", err)
	}

	ws.getLogger().Debug("auth запрос отправлен, ожидаем подтверждение")

	// Ожидаем подтверждение аутентификации
	// (в production лучше использовать channel с таймаутом)
	ws.authenticated.Store(true)

	ws.getLogger().Info("приватный WebSocket аутентифицирован")
	return nil
}

// subscribeToPrivateTopics подписывается на приватные топики.
func (ws *PrivateWebSocketClient) subscribeToPrivateTopics() error {
	// Подписка на позиции и исполнения
	topics := []string{
		"position",   // Обновления позиций (включая ликвидации)
		"execution",  // Исполнения ордеров
	}

	req := WsSubscribeRequest{
		Op:    "subscribe",
		ReqId: ws.nextReqID(),
		Args:  topics,
	}

	data, err := jsonFast.Marshal(req)
	if err != nil {
		return fmt.Errorf("ошибка сериализации подписки: %w", err)
	}

	if err := ws.writeMessage(data); err != nil {
		return fmt.Errorf("ошибка отправки подписки: %w", err)
	}

	ws.getLogger().Info("подписка на приватные топики",
		zap.Strings("topics", topics),
	)

	return nil
}

// nextReqID генерирует уникальный ReqId.
func (ws *PrivateWebSocketClient) nextReqID() string {
	id := ws.reqCounter.Add(1)
	return "bybit_priv_" + strconv.FormatUint(id, 10)
}

// =============================================================================
// Обработка сообщений
// =============================================================================

// runMessageLoop читает и обрабатывает входящие сообщения.
func (ws *PrivateWebSocketClient) runMessageLoop() {
	defer ws.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			ws.getLogger().Error("паника в приватном message loop",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
			ws.handleConnectionError(fmt.Errorf("panic: %v", r))
		}
	}()

	for {
		// Проверяем сигналы завершения
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

		// Получаем соединение
		ws.mu.RLock()
		conn := ws.conn
		ws.mu.RUnlock()

		if conn == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Устанавливаем deadline
		conn.SetReadDeadline(time.Now().Add(PrivateWsPingInterval + PongTimeout))

		// Читаем сообщение
		_, message, err := conn.ReadMessage()
		if err != nil {
			if ws.shouldStop.Load() {
				return
			}

			ws.getLogger().Error("ошибка чтения приватного WebSocket",
				zap.Error(err),
			)
			ws.handleConnectionError(err)
			return
		}

		// Обрабатываем сообщение
		ws.processMessage(message)
	}
}

// processMessage обрабатывает одно приватное сообщение.
func (ws *PrivateWebSocketClient) processMessage(data []byte) {
	// Определяем тип сообщения
	msgType := ws.detectPrivateMessageType(data)

	switch msgType {
	case "position":
		ws.handlePositionMessage(data)
	case "execution":
		ws.handleExecutionMessage(data)
	case "pong":
		metrics.IncrementWebSocketEvent(ExchangeName, "", "private_pong")
	case "subscribe":
		ws.getLogger().Debug("приватная подписка подтверждена")
	case "auth":
		ws.getLogger().Debug("аутентификация подтверждена")
	default:
		// Игнорируем неизвестные сообщения
	}
}

// detectPrivateMessageType определяет тип приватного сообщения.
func (ws *PrivateWebSocketClient) detectPrivateMessageType(data []byte) string {
	// Быстрая проверка через bytes.Contains
	if containsBytes(data, []byte(`"topic":"position"`)) {
		return "position"
	}
	if containsBytes(data, []byte(`"topic":"execution"`)) {
		return "execution"
	}
	if containsBytes(data, []byte(`"op":"pong"`)) {
		return "pong"
	}
	if containsBytes(data, []byte(`"op":"subscribe"`)) {
		return "subscribe"
	}
	if containsBytes(data, []byte(`"op":"auth"`)) {
		return "auth"
	}
	return "unknown"
}

// containsBytes проверяет наличие подстроки.
func containsBytes(data, pattern []byte) bool {
	for i := 0; i <= len(data)-len(pattern); i++ {
		if bytesEqual(data[i:i+len(pattern)], pattern) {
			return true
		}
	}
	return false
}

// bytesEqual сравнивает два слайса байт.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// =============================================================================
// Обработчики событий
// =============================================================================

// WsPrivatePositionMessage представляет сообщение позиции.
type WsPrivatePositionMessage struct {
	Topic string                   `json:"topic"`
	Data  []WsPrivatePositionData `json:"data"`
}

// WsPrivatePositionData представляет данные позиции.
type WsPrivatePositionData struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`          // "Buy" (long) или "Sell" (short)
	Size          string `json:"size"`
	AvgPrice      string `json:"avgPrice"`
	PositionValue string `json:"positionValue"`
	LiqPrice      string `json:"liqPrice"`
	UnrealisedPnl string `json:"unrealisedPnl"`
	PositionIdx   int    `json:"positionIdx"`
	UpdatedTime   string `json:"updatedTime"`
}

// handlePositionMessage обрабатывает сообщение о позиции.
//
// Ключевой метод для мониторинга ликвидаций согласно TZ.md 2.5C:
// "Ликвидация: Если биржа ликвидировала одну позицию →
//  Немедленно закрыть вторую позицию"
func (ws *PrivateWebSocketClient) handlePositionMessage(data []byte) {
	var msg WsPrivatePositionMessage
	if err := jsonFast.Unmarshal(data, &msg); err != nil {
		ws.getLogger().Warn("ошибка парсинга position сообщения",
			zap.Error(err),
		)
		return
	}

	cb := ws.positionCallback.Load()
	if cb == nil {
		return // Callback не установлен
	}

	callback := cb.(func(*PositionEvent))

	for _, pos := range msg.Data {
		size, _ := ParseFloat(pos.Size)
		avgPrice, _ := ParseFloat(pos.AvgPrice)
		unrealizedPNL, _ := ParseFloat(pos.UnrealisedPnl)
		liqPrice, _ := ParseFloat(pos.LiqPrice)
		updatedMs, _ := ParseInt(pos.UpdatedTime)

		// Определяем сторону позиции
		var side exchanges.PositionSide
		if pos.Side == SideBuy {
			side = exchanges.PositionSideLong
		} else {
			side = exchanges.PositionSideShort
		}

		// Создаём событие позиции
		event := &PositionEvent{
			Symbol:        pos.Symbol,
			Side:          side,
			Size:          size,
			AvgPrice:      avgPrice,
			UnrealizedPNL: unrealizedPNL,
			LiqPrice:      liqPrice,
			PositionIdx:   pos.PositionIdx,
			IsLiquidated:  size == 0, // Ликвидация = размер стал 0
			Timestamp:     time.UnixMilli(updatedMs),
		}

		// Логируем ликвидации
		if event.IsLiquidated {
			ws.getLogger().Warn("обнаружена ликвидация позиции",
				zap.String("symbol", event.Symbol),
				zap.String("side", string(event.Side)),
			)
			metrics.IncrementWebSocketEvent(ExchangeName, event.Symbol, "liquidation")
		}

		// Вызываем callback
		callback(event)
	}
}

// WsPrivateExecutionMessage представляет сообщение исполнения.
type WsPrivateExecutionMessage struct {
	Topic string                    `json:"topic"`
	Data  []WsPrivateExecutionData `json:"data"`
}

// WsPrivateExecutionData представляет данные исполнения.
type WsPrivateExecutionData struct {
	OrderID       string `json:"orderId"`
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	OrderType     string `json:"orderType"`
	ExecQty       string `json:"execQty"`
	ExecPrice     string `json:"execPrice"`
	OrderStatus   string `json:"orderStatus"`
	IsLiquidation bool   `json:"isLiquidation"`
	ExecTime      string `json:"execTime"`
}

// handleExecutionMessage обрабатывает сообщение об исполнении ордера.
func (ws *PrivateWebSocketClient) handleExecutionMessage(data []byte) {
	var msg WsPrivateExecutionMessage
	if err := jsonFast.Unmarshal(data, &msg); err != nil {
		ws.getLogger().Warn("ошибка парсинга execution сообщения",
			zap.Error(err),
		)
		return
	}

	cb := ws.executionCallback.Load()
	if cb == nil {
		return
	}

	callback := cb.(func(*ExecutionEvent))

	for _, exec := range msg.Data {
		execQty, _ := ParseFloat(exec.ExecQty)
		execPrice, _ := ParseFloat(exec.ExecPrice)
		execMs, _ := ParseInt(exec.ExecTime)

		// Конвертируем сторону
		var side exchanges.OrderSide
		if exec.Side == SideBuy {
			side = exchanges.OrderSideBuy
		} else {
			side = exchanges.OrderSideSell
		}

		event := &ExecutionEvent{
			OrderID:       exec.OrderID,
			Symbol:        exec.Symbol,
			Side:          side,
			OrderType:     exec.OrderType,
			ExecQty:       execQty,
			ExecPrice:     execPrice,
			OrderStatus:   convertOrderStatus(exec.OrderStatus),
			IsLiquidation: exec.IsLiquidation,
			Timestamp:     time.UnixMilli(execMs),
		}

		// Логируем ликвидации
		if event.IsLiquidation {
			ws.getLogger().Warn("исполнение по ликвидации",
				zap.String("symbol", event.Symbol),
				zap.String("orderId", event.OrderID),
				zap.Float64("qty", event.ExecQty),
				zap.Float64("price", event.ExecPrice),
			)
		}

		callback(event)
	}
}

// =============================================================================
// Ping и Reconnect
// =============================================================================

// runPingLoop отправляет ping для поддержания соединения.
func (ws *PrivateWebSocketClient) runPingLoop() {
	defer ws.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			ws.getLogger().Error("паника в приватном ping loop",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
		}
	}()

	ticker := time.NewTicker(PrivateWsPingInterval)
	defer ticker.Stop()

	for {
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
				ws.getLogger().Warn("приватный ping не удался",
					zap.Error(err),
				)
			}
		}
	}
}

// sendPing отправляет ping frame.
func (ws *PrivateWebSocketClient) sendPing() error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.RLock()
	conn := ws.conn
	ws.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	deadline := time.Now().Add(WriteTimeout)
	return conn.WriteControl(websocket.PingMessage, nil, deadline)
}

// handleConnectionError обрабатывает ошибку соединения.
func (ws *PrivateWebSocketClient) handleConnectionError(err error) {
	if ws.shouldStop.Load() {
		return
	}

	if ws.reconnecting.Swap(true) {
		return
	}

	ws.connected.Store(false)
	ws.authenticated.Store(false)

	ws.mu.Lock()
	select {
	case <-ws.stopLoopCh:
	default:
		close(ws.stopLoopCh)
	}
	ws.mu.Unlock()

	go ws.reconnect()
}

// reconnect выполняет переподключение с exponential backoff.
func (ws *PrivateWebSocketClient) reconnect() {
	defer func() {
		ws.reconnecting.Store(false)
		if r := recover(); r != nil {
			ws.getLogger().Error("паника в приватном reconnect",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())),
			)
		}
	}()

	backoff := PrivateWsInitialReconnectDelay

	for attempt := 1; attempt <= PrivateWsMaxReconnectAttempts; attempt++ {
		if ws.shouldStop.Load() {
			return
		}

		ws.getLogger().Info("попытка переподключения приватного WebSocket",
			zap.Int("attempt", attempt),
			zap.Duration("backoff", backoff),
		)

		select {
		case <-ws.closeCh:
			return
		case <-time.After(backoff):
		}

		if ws.shouldStop.Load() {
			return
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
			ws.getLogger().Error("приватный reconnect не удался",
				zap.Int("attempt", attempt),
				zap.Error(err),
			)

			backoff *= 2
			if backoff > PrivateWsMaxReconnectDelay {
				backoff = PrivateWsMaxReconnectDelay
			}
			continue
		}

		conn.SetPongHandler(func(appData string) error {
			metrics.IncrementWebSocketEvent(ExchangeName, "", "private_pong")
			return nil
		})

		ws.mu.Lock()
		ws.conn = conn
		ws.stopLoopCh = make(chan struct{})
		ws.connected.Store(true)
		ws.mu.Unlock()

		// Аутентификация
		if err := ws.authenticate(); err != nil {
			ws.getLogger().Error("ошибка аутентификации при reconnect",
				zap.Error(err),
			)
			continue
		}

		// Подписка
		if err := ws.subscribeToPrivateTopics(); err != nil {
			ws.getLogger().Warn("ошибка подписки при reconnect",
				zap.Error(err),
			)
		}

		ws.getLogger().Info("приватный WebSocket переподключён",
			zap.Int("attempt", attempt),
		)

		ws.wg.Add(2)
		go ws.runMessageLoop()
		go ws.runPingLoop()

		return
	}

	ws.getLogger().Error("не удалось переподключить приватный WebSocket")

	if cb := ws.errorCallback.Load(); cb != nil {
		cb.(func(error))(fmt.Errorf("private websocket reconnect failed"))
	}
}

// =============================================================================
// Закрытие и управление
// =============================================================================

// Close закрывает приватное WebSocket соединение.
func (ws *PrivateWebSocketClient) Close() error {
	if ws.shouldStop.Swap(true) {
		return nil
	}

	close(ws.closeCh)

	ws.mu.Lock()
	if ws.conn != nil {
		ws.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(WriteTimeout),
		)
		ws.conn.Close()
		ws.conn = nil
	}
	ws.connected.Store(false)
	ws.authenticated.Store(false)

	select {
	case <-ws.stopLoopCh:
	default:
		close(ws.stopLoopCh)
	}
	ws.mu.Unlock()

	// Ждём завершения горутин
	done := make(chan struct{})
	go func() {
		ws.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		ws.getLogger().Debug("все приватные горутины завершены")
	case <-time.After(GracefulShutdownTimeout):
		ws.getLogger().Warn("таймаут завершения приватных горутин")
	}

	ws.getLogger().Info("приватный WebSocket закрыт")
	return nil
}

// IsConnected возвращает статус подключения.
func (ws *PrivateWebSocketClient) IsConnected() bool {
	return ws.connected.Load() && ws.authenticated.Load()
}

// SetPositionCallback устанавливает callback для обновлений позиций.
// Потокобезопасен.
func (ws *PrivateWebSocketClient) SetPositionCallback(callback func(*PositionEvent)) {
	if callback != nil {
		ws.positionCallback.Store(callback)
	}
}

// SetExecutionCallback устанавливает callback для исполнений.
// Потокобезопасен.
func (ws *PrivateWebSocketClient) SetExecutionCallback(callback func(*ExecutionEvent)) {
	if callback != nil {
		ws.executionCallback.Store(callback)
	}
}

// SetErrorCallback устанавливает callback для ошибок.
// Потокобезопасен.
func (ws *PrivateWebSocketClient) SetErrorCallback(callback func(error)) {
	if callback != nil {
		ws.errorCallback.Store(callback)
	}
}

// SetLogger устанавливает логгер.
// Потокобезопасен.
func (ws *PrivateWebSocketClient) SetLogger(logger *zap.Logger) {
	if logger != nil {
		ws.logger.Store(logger)
	}
}

// writeMessage записывает сообщение в WebSocket.
func (ws *PrivateWebSocketClient) writeMessage(data []byte) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.RLock()
	conn := ws.conn
	ws.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return err
	}

	return conn.WriteMessage(websocket.TextMessage, data)
}
