package exchanges

import (
	"time"
)

// Exchange представляет общий интерфейс для работы с любой биржей.
// Все коннекторы бирж (Bybit, Bitget, BingX, Gate.io, OKX, HTX, MEXC) должны реализовывать этот интерфейс.
type Exchange interface {
	// PlaceOrder выставляет ордер на бирже
	PlaceOrder(req *OrderRequest) (*OrderResponse, error)

	// GetBalance возвращает баланс указанного актива
	GetBalance(asset string) (*Balance, error)

	// SetLeverage устанавливает плечо для указанного символа
	SetLeverage(symbol string, leverage int) error

	// Connect подключается к бирже (REST + WebSocket)
	Connect() error

	// Close закрывает все соединения с биржей
	Close() error

	// GetName возвращает название биржи (например, "bybit")
	GetName() string
}

// OrderRequest представляет запрос на выставление ордера.
type OrderRequest struct {
	Symbol       string      // Символ торговой пары (например, "BTCUSDT")
	Side         OrderSide   // Направление (buy/sell)
	Type         OrderType   // Тип ордера (market/limit)
	Quantity     float64     // Количество актива
	Price        *float64    // Цена (только для limit ордеров)
	PositionSide PositionSide // Сторона позиции (long/short)
}

// OrderResponse представляет ответ на выставление ордера.
type OrderResponse struct {
	OrderID      string      // ID ордера на бирже
	ClientID     string      // Клиентский ID (если задан)
	Symbol       string      // Символ торговой пары
	Side         OrderSide   // Направление (buy/sell)
	PositionSide PositionSide // Сторона позиции (long/short)
	FilledQty    float64     // Исполненное количество
	AvgPrice     float64     // Средняя цена исполнения
	Status       OrderStatus // Статус ордера
	CreatedAt    time.Time   // Время создания ордера
	Exchange     string      // Название биржи (для rollback logic)
}

// Balance представляет баланс актива на бирже.
type Balance struct {
	Asset      string  // Название актива (например, "USDT")
	Available  float64 // Доступный баланс (не в позициях)
	InPosition float64 // Баланс в открытых позициях
	Total      float64 // Общий баланс (Available + InPosition)
}

// OrderSide определяет направление ордера.
type OrderSide string

const (
	OrderSideBuy  OrderSide = "buy"  // Покупка (открытие long или закрытие short)
	OrderSideSell OrderSide = "sell" // Продажа (закрытие long или открытие short)
)

// OrderType определяет тип ордера.
type OrderType string

const (
	OrderTypeMarket OrderType = "market" // Рыночный ордер (исполняется немедленно по текущей цене)
	OrderTypeLimit  OrderType = "limit"  // Лимитный ордер (исполняется по заданной цене или лучше)
)

// PositionSide определяет сторону позиции (hedge mode).
type PositionSide string

const (
	PositionSideLong  PositionSide = "long"  // Long позиция (ставка на рост)
	PositionSideShort PositionSide = "short" // Short позиция (ставка на падение)
)

// OrderStatus определяет статус ордера.
type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "new"              // Ордер создан, но не исполнен
	OrderStatusPartiallyFilled OrderStatus = "partially_filled" // Ордер частично исполнен
	OrderStatusFilled          OrderStatus = "filled"           // Ордер полностью исполнен
	OrderStatusCancelled       OrderStatus = "cancelled"        // Ордер отменён
	OrderStatusRejected        OrderStatus = "rejected"         // Ордер отклонён биржей
)

// PriceUpdate представляет обновление цены из WebSocket.
// Используется для передачи данных от коннекторов бирж к Aggregator.
type PriceUpdate struct {
	Exchange  string     // Название биржи
	Symbol    string     // Символ торговой пары
	BestBid   float64    // Лучшая цена покупки
	BestAsk   float64    // Лучшая цена продажи
	BidQty    float64    // Объём на лучшем bid
	AskQty    float64    // Объём на лучшем ask
	Orderbook *OrderBook // Полный стакан (опционально, для расчёта средней цены)
	Timestamp time.Time  // Время обновления
}

// OrderBook представляет стакан ордеров (order book).
type OrderBook struct {
	Bids   []Level   // Уровни покупки (отсортированы по убыванию цены)
	Asks   []Level   // Уровни продажи (отсортированы по возрастанию цены)
	UpdateID int64   // ID обновления (для проверки актуальности)
}

// Level представляет один уровень в стакане.
type Level struct {
	Price    float64 // Цена
	Quantity float64 // Объём на этом уровне
}

// ExchangeInfo содержит метаинформацию о бирже.
// Загружается из configs/exchanges.json или через REST API.
type ExchangeInfo struct {
	Name            string                     // Название биржи
	MinOrderSizes   map[string]InstrumentLimits // Минимумы для каждого символа
	RateLimits      RateLimits                 // Лимиты запросов
}

// InstrumentLimits содержит ограничения для торгового инструмента.
type InstrumentLimits struct {
	MinQty   float64 // Минимальное количество для ордера
	MaxQty   float64 // Максимальное количество для ордера
	QtyStep  float64 // Шаг изменения количества (например, 0.001 BTC)
	TickSize float64 // Минимальный шаг цены (например, 0.5 USDT для BTC)
}

// RateLimits содержит лимиты запросов для биржи.
type RateLimits struct {
	RestPerMinute     int // Количество REST запросов в минуту
	WsSubscriptions   int // Максимальное количество подписок WebSocket
}

// ErrorType определяет тип ошибки от биржи (для retry logic).
type ErrorType string

const (
	ErrorTypeRateLimit          ErrorType = "rate_limit"           // Превышен лимит запросов
	ErrorTypeInsufficientMargin ErrorType = "insufficient_margin"  // Недостаточно маржи
	ErrorTypeInvalidSymbol      ErrorType = "invalid_symbol"       // Неверный символ
	ErrorTypeInvalidQuantity    ErrorType = "invalid_quantity"     // Неверное количество
	ErrorTypeNetworkError       ErrorType = "network_error"        // Сетевая ошибка
	ErrorTypeUnknown            ErrorType = "unknown"              // Неизвестная ошибка
)

// ExchangeError представляет ошибку от биржи с дополнительной информацией.
type ExchangeError struct {
	Type    ErrorType // Тип ошибки
	Code    int       // Код ошибки от биржи
	Message string    // Сообщение об ошибке
	Retry   bool      // Можно ли повторить запрос
}

func (e *ExchangeError) Error() string {
	return e.Message
}

// IsRateLimitError проверяет, является ли ошибка превышением лимита запросов.
func IsRateLimitError(err error) bool {
	if exchErr, ok := err.(*ExchangeError); ok {
		return exchErr.Type == ErrorTypeRateLimit
	}
	return false
}

// IsInsufficientMarginError проверяет, является ли ошибка недостатком маржи.
func IsInsufficientMarginError(err error) bool {
	if exchErr, ok := err.(*ExchangeError); ok {
		return exchErr.Type == ErrorTypeInsufficientMargin
	}
	return false
}

// IsRetryableError проверяет, можно ли повторить запрос после этой ошибки.
func IsRetryableError(err error) bool {
	if exchErr, ok := err.(*ExchangeError); ok {
		return exchErr.Retry
	}
	return false
}

// SymbolFormat определяет формат символа для каждой биржи.
// Разные биржи используют разные разделители и суффиксы.
type SymbolFormat struct {
	Separator string // Разделитель между base и quote (например, "-", "_", "")
	Suffix    string // Суффикс для контрактов (например, "-SWAP" для OKX)
}

// exchangeSymbolFormats содержит форматы символов для каждой биржи.
var exchangeSymbolFormats = map[string]SymbolFormat{
	"bybit":  {Separator: "", Suffix: ""},        // BTCUSDT
	"bitget": {Separator: "", Suffix: ""},        // BTCUSDT
	"bingx":  {Separator: "-", Suffix: ""},       // BTC-USDT
	"gate":   {Separator: "_", Suffix: ""},       // BTC_USDT
	"okx":    {Separator: "-", Suffix: "-SWAP"},  // BTC-USDT-SWAP
	"htx":    {Separator: "-", Suffix: ""},       // BTC-USDT
	"mexc":   {Separator: "_", Suffix: ""},       // BTC_USDT
}

// NormalizeSymbol преобразует символ в унифицированный формат (BTCUSDT).
// Используется для хранения и сравнения символов.
func NormalizeSymbol(symbol string) string {
	// Удалить известные суффиксы
	for _, suffix := range []string{"-SWAP", "_SWAP", "-PERP", "_PERP"} {
		if len(symbol) > len(suffix) && symbol[len(symbol)-len(suffix):] == suffix {
			symbol = symbol[:len(symbol)-len(suffix)]
		}
	}

	// Заменить разделители на пустую строку
	result := ""
	for _, r := range symbol {
		if r != '-' && r != '_' {
			result += string(r)
		}
	}

	return result
}

// FormatSymbolForExchange преобразует унифицированный символ (BTCUSDT) в формат биржи.
// Параметры:
//   - symbol: унифицированный символ (например, "BTCUSDT")
//   - exchange: название биржи (например, "okx")
//
// Возвращает символ в формате биржи (например, "BTC-USDT-SWAP" для OKX).
func FormatSymbolForExchange(symbol, exchange string) string {
	format, exists := exchangeSymbolFormats[exchange]
	if !exists {
		// По умолчанию возвращаем как есть
		return symbol
	}

	// Если разделитель пустой и нет суффикса - возвращаем как есть
	if format.Separator == "" && format.Suffix == "" {
		return symbol
	}

	// Извлечь base и quote из унифицированного символа
	// Предполагаем формат типа BTCUSDT, ETHUSDT и т.д.
	base, quote := extractBaseQuote(symbol)
	if base == "" || quote == "" {
		return symbol // Не удалось разобрать, возвращаем как есть
	}

	return base + format.Separator + quote + format.Suffix
}

// extractBaseQuote извлекает базовую и котируемую валюту из символа.
// Поддерживает стандартные котируемые валюты: USDT, USD, BUSD, USDC.
func extractBaseQuote(symbol string) (base, quote string) {
	// Известные котируемые валюты (в порядке приоритета проверки)
	quotes := []string{"USDT", "BUSD", "USDC", "USD"}

	for _, q := range quotes {
		if len(symbol) > len(q) && symbol[len(symbol)-len(q):] == q {
			return symbol[:len(symbol)-len(q)], q
		}
	}

	return "", ""
}
