// Package bybit реализует коннектор к бирже Bybit.
// Поддерживает Bybit V5 API для торговли USDT-маржинальными фьючерсами (linear).
package bybit

import (
	"encoding/json"
	"strconv"
	"time"
)

// =============================================================================
// Константы Bybit API
// =============================================================================

const (
	// ExchangeName — название биржи для идентификации.
	ExchangeName = "bybit"

	// RestBaseURL — базовый URL для REST API.
	RestBaseURL = "https://api.bybit.com"

	// WsPublicURL — URL для публичного WebSocket (рыночные данные).
	WsPublicURL = "wss://stream.bybit.com/v5/public/linear"

	// WsPrivateURL — URL для приватного WebSocket (ордера, позиции).
	WsPrivateURL = "wss://stream.bybit.com/v5/private"

	// CategoryLinear — категория для USDT-маржинальных фьючерсов.
	CategoryLinear = "linear"

	// CategoryInverse — категория для инверсных контрактов (не используется).
	CategoryInverse = "inverse"
)

// REST API эндпоинты.
const (
	// EndpointPlaceOrder — создание ордера.
	EndpointPlaceOrder = "/v5/order/create"

	// EndpointCancelOrder — отмена ордера.
	EndpointCancelOrder = "/v5/order/cancel"

	// EndpointGetOrders — получение списка ордеров.
	EndpointGetOrders = "/v5/order/realtime"

	// EndpointGetPositions — получение открытых позиций.
	EndpointGetPositions = "/v5/position/list"

	// EndpointSetLeverage — установка плеча.
	EndpointSetLeverage = "/v5/position/set-leverage"

	// EndpointGetWalletBalance — баланс кошелька.
	EndpointGetWalletBalance = "/v5/account/wallet-balance"

	// EndpointGetInstruments — информация о торговых инструментах.
	EndpointGetInstruments = "/v5/market/instruments-info"

	// EndpointGetTickers — текущие цены (тикеры).
	EndpointGetTickers = "/v5/market/tickers"
)

// Типы ордеров Bybit.
const (
	// OrderTypeMarket — рыночный ордер.
	OrderTypeMarket = "Market"

	// OrderTypeLimit — лимитный ордер.
	OrderTypeLimit = "Limit"
)

// Направления ордеров Bybit.
const (
	// SideBuy — покупка.
	SideBuy = "Buy"

	// SideSell — продажа.
	SideSell = "Sell"
)

// Стороны позиции (Position Side) для Hedge Mode.
const (
	// PositionSideLong — длинная позиция.
	PositionSideLong = "Buy"

	// PositionSideShort — короткая позиция.
	PositionSideShort = "Sell"
)

// Time In Force параметры.
const (
	// TIFGoodTillCancel — ордер активен до отмены.
	TIFGoodTillCancel = "GTC"

	// TIFImmediateOrCancel — исполнить немедленно или отменить остаток.
	TIFImmediateOrCancel = "IOC"

	// TIFFillOrKill — полное исполнение или отмена.
	TIFFillOrKill = "FOK"
)

// =============================================================================
// Коды ошибок Bybit
// =============================================================================

// Коды успешных ответов.
const (
	// RetCodeSuccess — успешный запрос.
	RetCodeSuccess = 0
)

// Коды ошибок, требующих специальной обработки.
const (
	// ErrCodeRateLimit — превышен лимит запросов.
	ErrCodeRateLimit = 10006

	// ErrCodeInsufficientBalance — недостаточно баланса.
	ErrCodeInsufficientBalance = 110007

	// ErrCodeInvalidSymbol — неверный символ.
	ErrCodeInvalidSymbol = 10001

	// ErrCodeOrderNotFound — ордер не найден.
	ErrCodeOrderNotFound = 110001

	// ErrCodePositionNotExists — позиция не существует.
	ErrCodePositionNotExists = 110009

	// ErrCodeLeverageNotChanged — плечо уже установлено на это значение.
	ErrCodeLeverageNotChanged = 110043

	// ErrCodeInvalidQty — неверное количество.
	ErrCodeInvalidQty = 10003

	// ErrCodeReduceOnlyError — ошибка reduce only.
	ErrCodeReduceOnlyError = 110017
)

// =============================================================================
// Общие типы REST API
// =============================================================================

// APIResponse представляет общий формат ответа Bybit REST API.
// Все ответы содержат retCode, retMsg и result.
type APIResponse[T any] struct {
	RetCode    int    `json:"retCode"`    // Код возврата (0 = успех)
	RetMsg     string `json:"retMsg"`     // Сообщение (обычно "OK" при успехе)
	Result     T      `json:"result"`     // Данные ответа (зависят от эндпоинта)
	RetExtInfo any    `json:"retExtInfo"` // Дополнительная информация
	Time       int64  `json:"time"`       // Время ответа сервера (unix ms)
}

// APIResponseRaw представляет ответ с сырыми данными (для отладки).
type APIResponseRaw struct {
	RetCode    int             `json:"retCode"`
	RetMsg     string          `json:"retMsg"`
	Result     json.RawMessage `json:"result"`
	RetExtInfo any             `json:"retExtInfo"`
	Time       int64           `json:"time"`
}

// =============================================================================
// Типы для Place Order
// =============================================================================

// PlaceOrderRequest представляет запрос на создание ордера.
type PlaceOrderRequest struct {
	Category     string  `json:"category"`               // Категория: "linear"
	Symbol       string  `json:"symbol"`                 // Символ: "BTCUSDT"
	Side         string  `json:"side"`                   // Направление: "Buy" или "Sell"
	OrderType    string  `json:"orderType"`              // Тип: "Market" или "Limit"
	Qty          string  `json:"qty"`                    // Количество (строка для точности)
	Price        string  `json:"price,omitempty"`        // Цена (только для Limit)
	PositionIdx  int     `json:"positionIdx"`            // Индекс позиции: 0=one-way, 1=buy/long, 2=sell/short
	TimeInForce  string  `json:"timeInForce,omitempty"`  // TIF: "GTC", "IOC", "FOK"
	OrderLinkId  string  `json:"orderLinkId,omitempty"`  // Клиентский ID ордера
	ReduceOnly   bool    `json:"reduceOnly,omitempty"`   // Только уменьшение позиции
	CloseOnTrigger bool  `json:"closeOnTrigger,omitempty"` // Закрыть при срабатывании
}

// PlaceOrderResult представляет результат создания ордера.
type PlaceOrderResult struct {
	OrderID     string `json:"orderId"`     // ID ордера на бирже
	OrderLinkId string `json:"orderLinkId"` // Клиентский ID
}

// =============================================================================
// Типы для Get Order / Order Status
// =============================================================================

// GetOrdersResult представляет список ордеров.
type GetOrdersResult struct {
	Category       string      `json:"category"`
	List           []OrderInfo `json:"list"`
	NextPageCursor string      `json:"nextPageCursor"`
}

// OrderInfo представляет информацию об ордере.
type OrderInfo struct {
	OrderID            string `json:"orderId"`            // ID ордера
	OrderLinkId        string `json:"orderLinkId"`        // Клиентский ID
	Symbol             string `json:"symbol"`             // Символ
	Side               string `json:"side"`               // Направление
	OrderType          string `json:"orderType"`          // Тип ордера
	Price              string `json:"price"`              // Цена
	Qty                string `json:"qty"`                // Запрошенное количество
	CumExecQty         string `json:"cumExecQty"`         // Исполненное количество
	CumExecValue       string `json:"cumExecValue"`       // Исполненная стоимость
	AvgPrice           string `json:"avgPrice"`           // Средняя цена исполнения
	OrderStatus        string `json:"orderStatus"`        // Статус ордера
	TimeInForce        string `json:"timeInForce"`        // TIF
	PositionIdx        int    `json:"positionIdx"`        // Индекс позиции
	ReduceOnly         bool   `json:"reduceOnly"`         // Reduce only
	CreatedTime        string `json:"createdTime"`        // Время создания (unix ms)
	UpdatedTime        string `json:"updatedTime"`        // Время обновления
	RejectReason       string `json:"rejectReason"`       // Причина отклонения
	TriggerPrice       string `json:"triggerPrice"`       // Триггер-цена
	TakeProfit         string `json:"takeProfit"`         // Take Profit
	StopLoss           string `json:"stopLoss"`           // Stop Loss
	LeavesQty          string `json:"leavesQty"`          // Оставшееся количество
	LeavesValue        string `json:"leavesValue"`        // Оставшаяся стоимость
	CumExecFee         string `json:"cumExecFee"`         // Комиссия
	PlaceType          string `json:"placeType"`          // Тип размещения
	SmpType            string `json:"smpType"`            // Self-match prevention
	SmpGroup           int    `json:"smpGroup"`           // Группа SMP
	SmpOrderId         string `json:"smpOrderId"`         // ID SMP ордера
}

// Статусы ордеров Bybit.
const (
	// OrderStatusNew — новый ордер (ожидает исполнения).
	OrderStatusNew = "New"

	// OrderStatusPartiallyFilled — частично исполнен.
	OrderStatusPartiallyFilled = "PartiallyFilled"

	// OrderStatusFilled — полностью исполнен.
	OrderStatusFilled = "Filled"

	// OrderStatusCancelled — отменён.
	OrderStatusCancelled = "Cancelled"

	// OrderStatusRejected — отклонён.
	OrderStatusRejected = "Rejected"

	// OrderStatusDeactivated — деактивирован (для conditional orders).
	OrderStatusDeactivated = "Deactivated"
)

// =============================================================================
// Типы для Balance
// =============================================================================

// GetWalletBalanceResult представляет результат запроса баланса.
type GetWalletBalanceResult struct {
	List []AccountInfo `json:"list"`
}

// AccountInfo представляет информацию об аккаунте.
type AccountInfo struct {
	AccountType            string     `json:"accountType"`            // Тип аккаунта: "UNIFIED", "CONTRACT"
	AccountIMRate          string     `json:"accountIMRate"`          // Initial Margin Rate
	AccountMMRate          string     `json:"accountMMRate"`          // Maintenance Margin Rate
	TotalEquity            string     `json:"totalEquity"`            // Общий эквити
	TotalWalletBalance     string     `json:"totalWalletBalance"`     // Общий баланс кошелька
	TotalMarginBalance     string     `json:"totalMarginBalance"`     // Общий маржинальный баланс
	TotalAvailableBalance  string     `json:"totalAvailableBalance"`  // Доступный баланс
	TotalPerpUPL           string     `json:"totalPerpUPL"`           // Нереализованный PNL фьючерсов
	TotalInitialMargin     string     `json:"totalInitialMargin"`     // Начальная маржа
	TotalMaintenanceMargin string     `json:"totalMaintenanceMargin"` // Поддерживающая маржа
	Coin                   []CoinInfo `json:"coin"`                   // Информация по монетам
}

// CoinInfo представляет информацию о балансе конкретной монеты.
type CoinInfo struct {
	Coin                string `json:"coin"`                // Название монеты: "USDT"
	Equity              string `json:"equity"`              // Эквити
	UsdValue            string `json:"usdValue"`            // USD стоимость
	WalletBalance       string `json:"walletBalance"`       // Баланс кошелька
	AvailableToWithdraw string `json:"availableToWithdraw"` // Доступно для вывода
	AvailableToBorrow   string `json:"availableToBorrow"`   // Доступно для займа
	BorrowAmount        string `json:"borrowAmount"`        // Сумма займа
	AccruedInterest     string `json:"accruedInterest"`     // Накопленный процент
	TotalOrderIM        string `json:"totalOrderIM"`        // Initial Margin ордеров
	TotalPositionIM     string `json:"totalPositionIM"`     // Initial Margin позиций
	TotalPositionMM     string `json:"totalPositionMM"`     // Maintenance Margin позиций
	UnrealisedPnl       string `json:"unrealisedPnl"`       // Нереализованный PNL
	CumRealisedPnl      string `json:"cumRealisedPnl"`      // Накопленный реализованный PNL
	Bonus               string `json:"bonus"`               // Бонус
}

// =============================================================================
// Типы для Leverage
// =============================================================================

// SetLeverageRequest представляет запрос на установку плеча.
type SetLeverageRequest struct {
	Category     string `json:"category"`     // Категория: "linear"
	Symbol       string `json:"symbol"`       // Символ: "BTCUSDT"
	BuyLeverage  string `json:"buyLeverage"`  // Плечо для long позиций
	SellLeverage string `json:"sellLeverage"` // Плечо для short позиций
}

// SetLeverageResult представляет результат установки плеча (пустой при успехе).
type SetLeverageResult struct {
	// Bybit возвращает пустой объект при успехе
}

// =============================================================================
// Типы для Positions
// =============================================================================

// GetPositionsResult представляет результат запроса позиций.
type GetPositionsResult struct {
	Category       string         `json:"category"`
	List           []PositionInfo `json:"list"`
	NextPageCursor string         `json:"nextPageCursor"`
}

// PositionInfo представляет информацию о позиции.
type PositionInfo struct {
	Symbol           string `json:"symbol"`           // Символ
	Side             string `json:"side"`             // Сторона: "Buy" (long) или "Sell" (short)
	Size             string `json:"size"`             // Размер позиции
	AvgPrice         string `json:"avgPrice"`         // Средняя цена входа
	PositionValue    string `json:"positionValue"`    // Стоимость позиции
	PositionIdx      int    `json:"positionIdx"`      // Индекс позиции
	Leverage         string `json:"leverage"`         // Текущее плечо
	MarkPrice        string `json:"markPrice"`        // Маркировочная цена
	LiqPrice         string `json:"liqPrice"`         // Цена ликвидации
	PositionIM       string `json:"positionIM"`       // Initial Margin позиции
	PositionMM       string `json:"positionMM"`       // Maintenance Margin
	UnrealisedPnl    string `json:"unrealisedPnl"`    // Нереализованный PNL
	CumRealisedPnl   string `json:"cumRealisedPnl"`   // Накопленный реализованный PNL
	TpSlMode         string `json:"tpSlMode"`         // Режим TP/SL
	TakeProfit       string `json:"takeProfit"`       // Take Profit
	StopLoss         string `json:"stopLoss"`         // Stop Loss
	TrailingStop     string `json:"trailingStop"`     // Trailing Stop
	CreatedTime      string `json:"createdTime"`      // Время создания
	UpdatedTime      string `json:"updatedTime"`      // Время обновления
	RiskId           int    `json:"riskId"`           // ID риска
	RiskLimitValue   string `json:"riskLimitValue"`   // Лимит риска
	PositionStatus   string `json:"positionStatus"`   // Статус позиции
	AutoAddMargin    int    `json:"autoAddMargin"`    // Авто-добавление маржи
	PositionBalance  string `json:"positionBalance"`  // Баланс позиции
	BustPrice        string `json:"bustPrice"`        // Цена банкротства
	AdlRankIndicator int    `json:"adlRankIndicator"` // Индикатор ADL
	IsReduceOnly     bool   `json:"isReduceOnly"`     // Только уменьшение
	MmrSysUpdatedTime string `json:"mmrSysUpdatedTime"` // Время обновления MMR
	LeverageSysUpdatedTime string `json:"leverageSysUpdatedTime"` // Время обновления плеча
}

// =============================================================================
// Типы для Instrument Info
// =============================================================================

// GetInstrumentsResult представляет результат запроса инструментов.
type GetInstrumentsResult struct {
	Category       string           `json:"category"`
	List           []InstrumentInfo `json:"list"`
	NextPageCursor string           `json:"nextPageCursor"`
}

// InstrumentInfo представляет информацию о торговом инструменте.
type InstrumentInfo struct {
	Symbol          string          `json:"symbol"`          // Символ: "BTCUSDT"
	ContractType    string          `json:"contractType"`    // Тип контракта: "LinearPerpetual"
	Status          string          `json:"status"`          // Статус: "Trading"
	BaseCoin        string          `json:"baseCoin"`        // Базовая валюта: "BTC"
	QuoteCoin       string          `json:"quoteCoin"`       // Котируемая валюта: "USDT"
	LaunchTime      string          `json:"launchTime"`      // Время запуска
	DeliveryTime    string          `json:"deliveryTime"`    // Время поставки
	DeliveryFeeRate string          `json:"deliveryFeeRate"` // Комиссия поставки
	PriceScale      string          `json:"priceScale"`      // Масштаб цены
	LeverageFilter  LeverageFilter  `json:"leverageFilter"`  // Фильтр плеча
	PriceFilter     PriceFilter     `json:"priceFilter"`     // Фильтр цены
	LotSizeFilter   LotSizeFilter   `json:"lotSizeFilter"`   // Фильтр размера лота
	UnifiedMarginTrade bool         `json:"unifiedMarginTrade"` // Унифицированная маржа
	FundingInterval int             `json:"fundingInterval"` // Интервал фандинга
	SettleCoin      string          `json:"settleCoin"`      // Валюта расчётов
	CopyTrading     string          `json:"copyTrading"`     // Копи-трейдинг
}

// LeverageFilter содержит ограничения по плечу.
type LeverageFilter struct {
	MinLeverage  string `json:"minLeverage"`  // Минимальное плечо: "1"
	MaxLeverage  string `json:"maxLeverage"`  // Максимальное плечо: "100"
	LeverageStep string `json:"leverageStep"` // Шаг плеча: "0.01"
}

// PriceFilter содержит ограничения по цене.
type PriceFilter struct {
	MinPrice string `json:"minPrice"` // Минимальная цена
	MaxPrice string `json:"maxPrice"` // Максимальная цена
	TickSize string `json:"tickSize"` // Шаг цены (tick size)
}

// LotSizeFilter содержит ограничения по размеру ордера.
type LotSizeFilter struct {
	MinOrderQty       string `json:"minOrderQty"`       // Минимальный объём ордера
	MaxOrderQty       string `json:"maxOrderQty"`       // Максимальный объём ордера
	QtyStep           string `json:"qtyStep"`           // Шаг объёма
	PostOnlyMaxOrderQty string `json:"postOnlyMaxOrderQty"` // Макс. объём для post-only
	MaxMktOrderQty    string `json:"maxMktOrderQty"`    // Макс. объём для market ордера
	MinNotionalValue  string `json:"minNotionalValue"`  // Минимальная стоимость ордера
}

// =============================================================================
// WebSocket типы — Общие
// =============================================================================

// WsMessage представляет общее WebSocket сообщение от Bybit.
type WsMessage struct {
	Topic     string          `json:"topic,omitempty"`     // Топик (например, "orderbook.50.BTCUSDT")
	Type      string          `json:"type,omitempty"`      // Тип: "snapshot", "delta"
	Timestamp int64           `json:"ts,omitempty"`        // Timestamp сервера
	Data      json.RawMessage `json:"data,omitempty"`      // Данные (зависят от топика)

	// Для служебных сообщений
	Success bool   `json:"success,omitempty"` // Успех операции
	RetMsg  string `json:"ret_msg,omitempty"` // Сообщение
	Op      string `json:"op,omitempty"`      // Операция: "subscribe", "pong"
	ConnId  string `json:"conn_id,omitempty"` // ID соединения
	ReqId   string `json:"req_id,omitempty"`  // ID запроса
}

// WsSubscribeRequest представляет запрос на подписку.
type WsSubscribeRequest struct {
	Op    string   `json:"op"`    // Операция: "subscribe"
	ReqId string   `json:"req_id,omitempty"` // ID запроса (опционально)
	Args  []string `json:"args"`  // Список топиков для подписки
}

// WsUnsubscribeRequest представляет запрос на отписку.
type WsUnsubscribeRequest struct {
	Op    string   `json:"op"`    // Операция: "unsubscribe"
	ReqId string   `json:"req_id,omitempty"`
	Args  []string `json:"args"`
}

// WsPingRequest представляет ping запрос.
type WsPingRequest struct {
	Op    string `json:"op"`     // Операция: "ping"
	ReqId string `json:"req_id,omitempty"`
}

// WsAuthRequest представляет запрос аутентификации для приватного WebSocket.
type WsAuthRequest struct {
	Op    string   `json:"op"`   // Операция: "auth"
	Args  []string `json:"args"` // [api_key, expires, signature]
	ReqId string   `json:"req_id,omitempty"`
}

// =============================================================================
// WebSocket типы — Orderbook
// =============================================================================

// WsOrderbookData представляет данные стакана из WebSocket.
type WsOrderbookData struct {
	Symbol   string     `json:"s"`  // Символ: "BTCUSDT"
	Bids     [][]string `json:"b"`  // Биды: [[price, qty], ...]
	Asks     [][]string `json:"a"`  // Аски: [[price, qty], ...]
	UpdateID int64      `json:"u"`  // ID обновления
	Seq      int64      `json:"seq"` // Sequence number
}

// WsOrderbookMessage представляет полное сообщение orderbook.
type WsOrderbookMessage struct {
	Topic     string          `json:"topic"` // "orderbook.50.BTCUSDT"
	Type      string          `json:"type"`  // "snapshot" или "delta"
	Timestamp int64           `json:"ts"`    // Timestamp
	Data      WsOrderbookData `json:"data"`  // Данные стакана
}

// =============================================================================
// WebSocket типы — Trade
// =============================================================================

// WsTradeData представляет данные о сделке из WebSocket.
type WsTradeData struct {
	Timestamp    int64  `json:"T"`  // Время сделки (unix ms)
	Symbol       string `json:"s"`  // Символ
	Side         string `json:"S"`  // Сторона: "Buy" или "Sell"
	Volume       string `json:"v"`  // Объём
	Price        string `json:"p"`  // Цена
	TickDirection string `json:"L"` // Направление тика: "PlusTick", "MinusTick", "ZeroPlusTick", "ZeroMinusTick"
	TradeID      string `json:"i"`  // ID сделки
	BlockTrade   bool   `json:"BT"` // Блочная сделка
}

// WsTradeMessage представляет сообщение о сделках.
type WsTradeMessage struct {
	Topic     string        `json:"topic"` // "publicTrade.BTCUSDT"
	Type      string        `json:"type"`  // "snapshot"
	Timestamp int64         `json:"ts"`
	Data      []WsTradeData `json:"data"`
}

// =============================================================================
// WebSocket типы — Ticker
// =============================================================================

// WsTickerData представляет данные тикера из WebSocket.
type WsTickerData struct {
	Symbol            string `json:"symbol"`            // Символ
	TickDirection     string `json:"tickDirection"`     // Направление тика
	Price24hPcnt      string `json:"price24hPcnt"`      // Изменение за 24ч в %
	LastPrice         string `json:"lastPrice"`         // Последняя цена
	PrevPrice24h      string `json:"prevPrice24h"`      // Цена 24ч назад
	HighPrice24h      string `json:"highPrice24h"`      // Максимум за 24ч
	LowPrice24h       string `json:"lowPrice24h"`       // Минимум за 24ч
	PrevPrice1h       string `json:"prevPrice1h"`       // Цена 1ч назад
	MarkPrice         string `json:"markPrice"`         // Маркировочная цена
	IndexPrice        string `json:"indexPrice"`        // Индексная цена
	OpenInterest      string `json:"openInterest"`      // Открытый интерес
	OpenInterestValue string `json:"openInterestValue"` // Стоимость OI
	Turnover24h       string `json:"turnover24h"`       // Оборот за 24ч
	Volume24h         string `json:"volume24h"`         // Объём за 24ч
	NextFundingTime   string `json:"nextFundingTime"`   // Время след. фандинга
	FundingRate       string `json:"fundingRate"`       // Ставка фандинга
	Bid1Price         string `json:"bid1Price"`         // Лучший бид (цена)
	Bid1Size          string `json:"bid1Size"`          // Лучший бид (объём)
	Ask1Price         string `json:"ask1Price"`         // Лучший аск (цена)
	Ask1Size          string `json:"ask1Size"`          // Лучший аск (объём)
}

// WsTickerMessage представляет сообщение тикера.
type WsTickerMessage struct {
	Topic     string       `json:"topic"` // "tickers.BTCUSDT"
	Type      string       `json:"type"`  // "snapshot" или "delta"
	Timestamp int64        `json:"ts"`
	CrossSeq  int64        `json:"cs"`
	Data      WsTickerData `json:"data"`
}

// =============================================================================
// Вспомогательные типы и функции
// =============================================================================

// Timestamp представляет время в миллисекундах (формат Bybit).
type Timestamp int64

// Time преобразует Timestamp в time.Time.
func (t Timestamp) Time() time.Time {
	return time.UnixMilli(int64(t))
}

// NewTimestamp создаёт Timestamp из time.Time.
func NewTimestamp(t time.Time) Timestamp {
	return Timestamp(t.UnixMilli())
}

// NowTimestamp возвращает текущее время как Timestamp.
func NowTimestamp() Timestamp {
	return Timestamp(time.Now().UnixMilli())
}

// ParseFloat парсит строку в float64 (используется для данных от Bybit).
// Bybit возвращает числа как строки для сохранения точности.
func ParseFloat(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// ParseInt парсит строку в int64.
func ParseInt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// FormatFloat форматирует float64 в строку для отправки в Bybit API.
func FormatFloat(f float64, precision int) string {
	return strconv.FormatFloat(f, 'f', precision, 64)
}

// FormatInt форматирует int в строку.
func FormatInt(i int) string {
	return strconv.Itoa(i)
}

