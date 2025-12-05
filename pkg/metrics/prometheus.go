package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// =====================================================================
// Метрики производительности
// =====================================================================

// TickToOrderDuration измеряет время от получения WebSocket тика до отправки ордера.
// Цель: < 5ms (критическое значение < 10ms).
// Labels: exchange, symbol
var TickToOrderDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "tick_to_order_duration_ms",
		Help:    "Время от получения тика до отправки ордера (мс)",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 20, 50, 100},
	},
	[]string{"exchange", "symbol"},
)

// SpreadCalculationDuration измеряет время расчёта спреда.
// Цель: < 0.5ms (критическое значение < 1ms).
// Labels: symbol
var SpreadCalculationDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "spread_calculation_duration_ms",
		Help:    "Время расчёта спреда (мс)",
		Buckets: []float64{0.01, 0.05, 0.1, 0.2, 0.5, 1, 2},
	},
	[]string{"symbol"},
)

// OrderExecutionDuration измеряет время исполнения ордера на бирже.
// Включает сетевую задержку + время обработки биржей.
// Labels: exchange, symbol, side, status
var OrderExecutionDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "order_execution_duration_ms",
		Help:    "Время исполнения ордера на бирже (мс)",
		Buckets: []float64{10, 25, 50, 100, 200, 500, 1000, 2000, 5000},
	},
	[]string{"exchange", "symbol", "side", "status"},
)

// JSONParsingDuration измеряет время парсинга JSON сообщений.
// Цель: < 0.1ms (критическое значение < 0.5ms).
// Labels: exchange, message_type
var JSONParsingDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "json_parsing_duration_ms",
		Help:    "Время парсинга JSON сообщения (мс)",
		Buckets: []float64{0.01, 0.05, 0.1, 0.2, 0.5, 1},
	},
	[]string{"exchange", "message_type"},
)

// PriceUpdateProcessingDuration измеряет время обработки обновления цены.
// Цель: < 1ms (критическое значение < 2ms).
// Labels: symbol
var PriceUpdateProcessingDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "price_update_processing_duration_ms",
		Help:    "Время обработки обновления цены (мс)",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10},
	},
	[]string{"symbol"},
)

// =====================================================================
// Счётчики событий
// =====================================================================

// WebSocketEventsTotal подсчитывает общее количество WebSocket событий.
// Labels: exchange, symbol, event_type (orderbook, trade, error, ping, pong)
var WebSocketEventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "websocket_events_total",
		Help: "Общее количество WebSocket событий",
	},
	[]string{"exchange", "symbol", "event_type"},
)

// OrdersTotal подсчитывает общее количество выставленных ордеров.
// Labels: exchange, symbol, side (buy/sell), position_side (long/short), status (filled/rejected/cancelled)
var OrdersTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "orders_total",
		Help: "Общее количество выставленных ордеров",
	},
	[]string{"exchange", "symbol", "side", "position_side", "status"},
)

// ArbitrageCyclesTotal подсчитывает общее количество арбитражных циклов.
// Labels: symbol, result (success/stop_loss/liquidation/error)
var ArbitrageCyclesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "arbitrage_cycles_total",
		Help: "Общее количество завершённых арбитражных циклов",
	},
	[]string{"symbol", "result"},
)

// BufferOverflowTotal подсчитывает случаи переполнения буфера событий.
// Labels: shard_id
var BufferOverflowTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "buffer_overflow_total",
		Help: "Количество случаев переполнения буфера событий",
	},
	[]string{"shard_id"},
)

// WebSocketReconnectsTotal подсчитывает количество переподключений WebSocket.
// Labels: exchange, success (true/false)
var WebSocketReconnectsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "websocket_reconnects_total",
		Help: "Количество переподключений WebSocket",
	},
	[]string{"exchange", "success"},
)

// APIErrorsTotal подсчитывает ошибки REST API запросов.
// Labels: exchange, endpoint, error_type (rate_limit/insufficient_margin/network/other)
var APIErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "api_errors_total",
		Help: "Количество ошибок REST API",
	},
	[]string{"exchange", "endpoint", "error_type"},
)

// =====================================================================
// Gauge метрики (текущее состояние)
// =====================================================================

// ActiveArbitrages показывает текущее количество активных арбитражей.
var ActiveArbitrages = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "active_arbitrages",
		Help: "Текущее количество активных арбитражей",
	},
)

// ActivePairs показывает количество активных торговых пар.
// Labels: status (ready/entering/position_open/exiting/paused/error)
var ActivePairs = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "active_pairs",
		Help: "Количество торговых пар по статусам",
	},
	[]string{"status"},
)

// WebSocketConnections показывает состояние WebSocket соединений.
// Labels: exchange
// Значения: 1 = подключено, 0 = отключено
var WebSocketConnections = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "websocket_connections",
		Help: "Состояние WebSocket соединений (1=подключено, 0=отключено)",
	},
	[]string{"exchange"},
)

// ExchangeBalance показывает баланс на биржах.
// Labels: exchange, asset (USDT, BTC, etc.)
var ExchangeBalance = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "exchange_balance",
		Help: "Баланс актива на бирже",
	},
	[]string{"exchange", "asset"},
)

// CurrentSpread показывает текущий спред для символа.
// Labels: symbol
var CurrentSpread = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "current_spread_percent",
		Help: "Текущий чистый спред в процентах",
	},
	[]string{"symbol"},
)

// UnrealizedPNL показывает нереализованный PNL позиции.
// Labels: symbol
var UnrealizedPNL = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "unrealized_pnl_usdt",
		Help: "Нереализованный PNL позиции в USDT",
	},
	[]string{"symbol"},
)

// TotalRealizedPNL показывает общий реализованный PNL.
var TotalRealizedPNL = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "total_realized_pnl_usdt",
		Help: "Общий реализованный PNL в USDT",
	},
)

// ChannelBufferSize показывает текущий размер буфера канала.
// Labels: shard_id
var ChannelBufferSize = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "channel_buffer_size",
		Help: "Текущее заполнение буфера канала событий",
	},
	[]string{"shard_id"},
)

// =====================================================================
// Summary метрики
// =====================================================================

// RealizedPNLSummary предоставляет статистику по реализованному PNL.
// Labels: symbol
var RealizedPNLSummary = prometheus.NewSummaryVec(
	prometheus.SummaryOpts{
		Name:       "realized_pnl_summary",
		Help:       "Статистика реализованного PNL по сделкам",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	},
	[]string{"symbol"},
)

// =====================================================================
// Регистрация метрик
// =====================================================================

// RegisterMetrics регистрирует все метрики в Prometheus registry.
// Вызывается один раз при инициализации приложения.
func RegisterMetrics() {
	// Гистограммы производительности
	prometheus.MustRegister(TickToOrderDuration)
	prometheus.MustRegister(SpreadCalculationDuration)
	prometheus.MustRegister(OrderExecutionDuration)
	prometheus.MustRegister(JSONParsingDuration)
	prometheus.MustRegister(PriceUpdateProcessingDuration)

	// Счётчики событий
	prometheus.MustRegister(WebSocketEventsTotal)
	prometheus.MustRegister(OrdersTotal)
	prometheus.MustRegister(ArbitrageCyclesTotal)
	prometheus.MustRegister(BufferOverflowTotal)
	prometheus.MustRegister(WebSocketReconnectsTotal)
	prometheus.MustRegister(APIErrorsTotal)

	// Gauge метрики
	prometheus.MustRegister(ActiveArbitrages)
	prometheus.MustRegister(ActivePairs)
	prometheus.MustRegister(WebSocketConnections)
	prometheus.MustRegister(ExchangeBalance)
	prometheus.MustRegister(CurrentSpread)
	prometheus.MustRegister(UnrealizedPNL)
	prometheus.MustRegister(TotalRealizedPNL)
	prometheus.MustRegister(ChannelBufferSize)

	// Summary метрики
	prometheus.MustRegister(RealizedPNLSummary)
}

// Handler возвращает HTTP handler для /metrics endpoint.
// Использовать с http.Handle("/metrics", metrics.Handler()).
func Handler() http.Handler {
	return promhttp.Handler()
}

// =====================================================================
// Вспомогательные функции
// =====================================================================

// Timer представляет таймер для измерения длительности операций.
// Используется для удобной записи метрик.
type Timer struct {
	startTime time.Time
}

// NewTimer создаёт новый таймер.
// Время начинается с момента вызова.
//
// Пример использования:
//
//	timer := metrics.NewTimer()
//	// ... выполнение операции ...
//	metrics.TickToOrderDuration.WithLabelValues("bybit", "BTCUSDT").Observe(timer.ElapsedMs())
func NewTimer() *Timer {
	return &Timer{
		startTime: time.Now(),
	}
}

// ElapsedMs возвращает прошедшее время в миллисекундах.
func (t *Timer) ElapsedMs() float64 {
	return float64(time.Since(t.startTime).Nanoseconds()) / 1_000_000
}

// Elapsed возвращает прошедшее время как time.Duration.
func (t *Timer) Elapsed() time.Duration {
	return time.Since(t.startTime)
}

// ObserveTickToOrder записывает метрику tick-to-order.
// Удобная обёртка для типичного use case.
func ObserveTickToOrder(exchange, symbol string, durationMs float64) {
	TickToOrderDuration.WithLabelValues(exchange, symbol).Observe(durationMs)
}

// ObserveSpreadCalculation записывает метрику расчёта спреда.
func ObserveSpreadCalculation(symbol string, durationMs float64) {
	SpreadCalculationDuration.WithLabelValues(symbol).Observe(durationMs)
}

// ObserveOrderExecution записывает метрику исполнения ордера.
func ObserveOrderExecution(exchange, symbol, side, status string, durationMs float64) {
	OrderExecutionDuration.WithLabelValues(exchange, symbol, side, status).Observe(durationMs)
}

// IncrementWebSocketEvent увеличивает счётчик WebSocket событий.
func IncrementWebSocketEvent(exchange, symbol, eventType string) {
	WebSocketEventsTotal.WithLabelValues(exchange, symbol, eventType).Inc()
}

// IncrementOrder увеличивает счётчик ордеров.
func IncrementOrder(exchange, symbol, side, positionSide, status string) {
	OrdersTotal.WithLabelValues(exchange, symbol, side, positionSide, status).Inc()
}

// IncrementArbitrageCycle увеличивает счётчик арбитражных циклов.
func IncrementArbitrageCycle(symbol, result string) {
	ArbitrageCyclesTotal.WithLabelValues(symbol, result).Inc()
}

// IncrementBufferOverflow увеличивает счётчик переполнений буфера.
func IncrementBufferOverflow(shardID string) {
	BufferOverflowTotal.WithLabelValues(shardID).Inc()
}

// IncrementReconnect увеличивает счётчик переподключений.
func IncrementReconnect(exchange string, success bool) {
	successStr := "false"
	if success {
		successStr = "true"
	}
	WebSocketReconnectsTotal.WithLabelValues(exchange, successStr).Inc()
}

// IncrementAPIError увеличивает счётчик ошибок API.
func IncrementAPIError(exchange, endpoint, errorType string) {
	APIErrorsTotal.WithLabelValues(exchange, endpoint, errorType).Inc()
}

// SetActiveArbitrages устанавливает количество активных арбитражей.
func SetActiveArbitrages(count int) {
	ActiveArbitrages.Set(float64(count))
}

// SetWebSocketConnected устанавливает статус WebSocket соединения.
func SetWebSocketConnected(exchange string, connected bool) {
	value := 0.0
	if connected {
		value = 1.0
	}
	WebSocketConnections.WithLabelValues(exchange).Set(value)
}

// SetExchangeBalance устанавливает баланс на бирже.
func SetExchangeBalance(exchange, asset string, balance float64) {
	ExchangeBalance.WithLabelValues(exchange, asset).Set(balance)
}

// SetCurrentSpread устанавливает текущий спред для символа.
func SetCurrentSpread(symbol string, spreadPercent float64) {
	CurrentSpread.WithLabelValues(symbol).Set(spreadPercent)
}

// SetUnrealizedPNL устанавливает нереализованный PNL.
func SetUnrealizedPNL(symbol string, pnl float64) {
	UnrealizedPNL.WithLabelValues(symbol).Set(pnl)
}

// AddRealizedPNL добавляет реализованный PNL к общему счётчику.
func AddRealizedPNL(pnl float64) {
	TotalRealizedPNL.Add(pnl)
}

// SetChannelBufferSize устанавливает текущий размер буфера канала.
func SetChannelBufferSize(shardID string, size int) {
	ChannelBufferSize.WithLabelValues(shardID).Set(float64(size))
}

// RecordRealizedPNL записывает PNL в summary для статистики.
func RecordRealizedPNL(symbol string, pnl float64) {
	RealizedPNLSummary.WithLabelValues(symbol).Observe(pnl)
}

// SetPairStatus устанавливает количество пар в указанном статусе.
func SetPairStatus(status string, count int) {
	ActivePairs.WithLabelValues(status).Set(float64(count))
}
