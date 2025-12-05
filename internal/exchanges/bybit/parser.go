package bybit

import (
	"fmt"
	"strings"
	"time"

	"arbitrage-terminal/internal/exchanges"

	jsoniter "github.com/json-iterator/go"
)

// =============================================================================
// Конфигурация jsoniter
// =============================================================================

// jsonFast — настроенный экземпляр jsoniter для максимальной производительности.
// Используется вместо стандартного encoding/json для достижения латентности < 0.1ms.
var jsonFast = jsoniter.ConfigCompatibleWithStandardLibrary

// =============================================================================
// Парсинг WebSocket сообщений
// =============================================================================

// MessageType определяет тип WebSocket сообщения.
type MessageType int

const (
	// MessageTypeUnknown — неизвестный тип сообщения.
	MessageTypeUnknown MessageType = iota
	// MessageTypeOrderbook — сообщение стакана.
	MessageTypeOrderbook
	// MessageTypeTicker — сообщение тикера.
	MessageTypeTicker
	// MessageTypeTrade — сообщение о сделках.
	MessageTypeTrade
	// MessageTypePong — ответ на ping.
	MessageTypePong
	// MessageTypeSubscribe — подтверждение подписки.
	MessageTypeSubscribe
	// MessageTypeAuth — ответ на аутентификацию.
	MessageTypeAuth
)

// DetectMessageType определяет тип WebSocket сообщения по его содержимому.
// Оптимизирован для минимальной аллокации — парсит только необходимые поля.
func DetectMessageType(data []byte) MessageType {
	// Быстрая проверка на служебные сообщения
	var base struct {
		Op    string `json:"op"`
		Topic string `json:"topic"`
	}

	if err := jsonFast.Unmarshal(data, &base); err != nil {
		return MessageTypeUnknown
	}

	// Проверяем служебные операции
	switch base.Op {
	case "pong":
		return MessageTypePong
	case "subscribe":
		return MessageTypeSubscribe
	case "auth":
		return MessageTypeAuth
	}

	// Определяем по топику
	if base.Topic == "" {
		return MessageTypeUnknown
	}

	switch {
	case strings.HasPrefix(base.Topic, "orderbook."):
		return MessageTypeOrderbook
	case strings.HasPrefix(base.Topic, "tickers."):
		return MessageTypeTicker
	case strings.HasPrefix(base.Topic, "publicTrade."):
		return MessageTypeTrade
	default:
		return MessageTypeUnknown
	}
}

// =============================================================================
// Парсинг Orderbook (с пулом объектов)
// =============================================================================

// ParseOrderbookMessagePooled парсит сообщение стакана с использованием пула объектов.
// Возвращает PriceUpdate из пула — вызывающий код ДОЛЖЕН вернуть его через
// exchanges.PutPriceUpdateWithOrderBook() после использования.
//
// Формат входных данных:
//
//	{
//	  "topic": "orderbook.50.BTCUSDT",
//	  "type": "snapshot",
//	  "ts": 1672304484978,
//	  "data": {
//	    "s": "BTCUSDT",
//	    "b": [["16500.50", "1.5"], ...],
//	    "a": [["16501.00", "2.0"], ...],
//	    "u": 123456,
//	    "seq": 789
//	  }
//	}
func ParseOrderbookMessagePooled(data []byte) (*exchanges.PriceUpdate, error) {
	var msg WsOrderbookMessage
	if err := jsonFast.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("failed to parse orderbook message: %w", err)
	}

	// Получаем объекты из пула
	update := exchanges.GetPriceUpdate()
	orderbook := exchanges.GetOrderBook()

	// Парсим уровни стакана напрямую в слайсы из пула
	if err := parseLevelsInto(msg.Data.Bids, &orderbook.Bids); err != nil {
		// Возвращаем объекты в пул при ошибке
		exchanges.PutOrderBook(orderbook)
		exchanges.PutPriceUpdate(update)
		return nil, fmt.Errorf("failed to parse bids: %w", err)
	}

	if err := parseLevelsInto(msg.Data.Asks, &orderbook.Asks); err != nil {
		exchanges.PutOrderBook(orderbook)
		exchanges.PutPriceUpdate(update)
		return nil, fmt.Errorf("failed to parse asks: %w", err)
	}

	orderbook.UpdateID = msg.Data.UpdateID

	// Извлекаем лучшие цены
	var bestBid, bestAsk, bidQty, askQty float64
	if len(orderbook.Bids) > 0 {
		bestBid = orderbook.Bids[0].Price
		bidQty = orderbook.Bids[0].Quantity
	}
	if len(orderbook.Asks) > 0 {
		bestAsk = orderbook.Asks[0].Price
		askQty = orderbook.Asks[0].Quantity
	}

	// Заполняем PriceUpdate
	update.Exchange = ExchangeName
	update.Symbol = msg.Data.Symbol
	update.BestBid = bestBid
	update.BestAsk = bestAsk
	update.BidQty = bidQty
	update.AskQty = askQty
	update.Orderbook = orderbook
	update.Timestamp = time.UnixMilli(msg.Timestamp)

	return update, nil
}

// ParseOrderbookMessage парсит сообщение стакана из WebSocket.
// DEPRECATED: Используйте ParseOrderbookMessagePooled для лучшей производительности.
func ParseOrderbookMessage(data []byte) (*exchanges.PriceUpdate, error) {
	var msg WsOrderbookMessage
	if err := jsonFast.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("failed to parse orderbook message: %w", err)
	}

	// Парсим уровни стакана
	bids, err := parseLevels(msg.Data.Bids)
	if err != nil {
		return nil, fmt.Errorf("failed to parse bids: %w", err)
	}

	asks, err := parseLevels(msg.Data.Asks)
	if err != nil {
		return nil, fmt.Errorf("failed to parse asks: %w", err)
	}

	// Извлекаем лучшие цены
	var bestBid, bestAsk, bidQty, askQty float64
	if len(bids) > 0 {
		bestBid = bids[0].Price
		bidQty = bids[0].Quantity
	}
	if len(asks) > 0 {
		bestAsk = asks[0].Price
		askQty = asks[0].Quantity
	}

	return &exchanges.PriceUpdate{
		Exchange:  ExchangeName,
		Symbol:    msg.Data.Symbol,
		BestBid:   bestBid,
		BestAsk:   bestAsk,
		BidQty:    bidQty,
		AskQty:    askQty,
		Orderbook: &exchanges.OrderBook{
			Bids:     bids,
			Asks:     asks,
			UpdateID: msg.Data.UpdateID,
		},
		Timestamp: time.UnixMilli(msg.Timestamp),
	}, nil
}

// parseLevelsInto преобразует уровни стакана напрямую в существующий слайс.
// Оптимизировано для минимизации аллокаций.
func parseLevelsInto(levels [][]string, target *[]exchanges.Level) error {
	if len(levels) == 0 {
		return nil
	}

	// Убеждаемся, что target имеет достаточную capacity
	// Это избегает переаллокации при append
	if cap(*target) < len(levels) {
		*target = make([]exchanges.Level, 0, len(levels))
	}

	for _, level := range levels {
		if len(level) < 2 {
			continue // Пропускаем некорректные уровни
		}

		price, err := ParseFloat(level[0])
		if err != nil {
			return fmt.Errorf("invalid price %q: %w", level[0], err)
		}

		qty, err := ParseFloat(level[1])
		if err != nil {
			return fmt.Errorf("invalid quantity %q: %w", level[1], err)
		}

		// Пропускаем уровни с нулевым объёмом (удалённые в delta)
		if qty == 0 {
			continue
		}

		*target = append(*target, exchanges.Level{
			Price:    price,
			Quantity: qty,
		})
	}

	return nil
}

// parseLevels преобразует уровни стакана из формата Bybit [[price, qty], ...]
// в слайс exchanges.Level.
func parseLevels(levels [][]string) ([]exchanges.Level, error) {
	if len(levels) == 0 {
		return nil, nil
	}

	// Предаллокация для производительности
	result := make([]exchanges.Level, 0, len(levels))

	for _, level := range levels {
		if len(level) < 2 {
			continue // Пропускаем некорректные уровни
		}

		price, err := ParseFloat(level[0])
		if err != nil {
			return nil, fmt.Errorf("invalid price %q: %w", level[0], err)
		}

		qty, err := ParseFloat(level[1])
		if err != nil {
			return nil, fmt.Errorf("invalid quantity %q: %w", level[1], err)
		}

		// Пропускаем уровни с нулевым объёмом (удалённые в delta)
		if qty == 0 {
			continue
		}

		result = append(result, exchanges.Level{
			Price:    price,
			Quantity: qty,
		})
	}

	return result, nil
}

// =============================================================================
// Парсинг Ticker (с пулом объектов)
// =============================================================================

// ParseTickerMessagePooled парсит сообщение тикера с использованием пула объектов.
// Возвращает PriceUpdate из пула — вызывающий код ДОЛЖЕН вернуть его через
// exchanges.PutPriceUpdate() после использования.
func ParseTickerMessagePooled(data []byte) (*exchanges.PriceUpdate, error) {
	var msg WsTickerMessage
	if err := jsonFast.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("failed to parse ticker message: %w", err)
	}

	// Получаем объект из пула
	update := exchanges.GetPriceUpdate()

	// Парсим цены
	bestBid, _ := ParseFloat(msg.Data.Bid1Price)
	bestAsk, _ := ParseFloat(msg.Data.Ask1Price)
	bidQty, _ := ParseFloat(msg.Data.Bid1Size)
	askQty, _ := ParseFloat(msg.Data.Ask1Size)

	// Заполняем PriceUpdate
	update.Exchange = ExchangeName
	update.Symbol = msg.Data.Symbol
	update.BestBid = bestBid
	update.BestAsk = bestAsk
	update.BidQty = bidQty
	update.AskQty = askQty
	update.Orderbook = nil // Тикер не содержит полный стакан
	update.Timestamp = time.UnixMilli(msg.Timestamp)

	return update, nil
}

// ParseTickerMessage парсит сообщение тикера из WebSocket.
// DEPRECATED: Используйте ParseTickerMessagePooled для лучшей производительности.
func ParseTickerMessage(data []byte) (*exchanges.PriceUpdate, error) {
	var msg WsTickerMessage
	if err := jsonFast.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("failed to parse ticker message: %w", err)
	}

	// Парсим цены
	bestBid, _ := ParseFloat(msg.Data.Bid1Price)
	bestAsk, _ := ParseFloat(msg.Data.Ask1Price)
	bidQty, _ := ParseFloat(msg.Data.Bid1Size)
	askQty, _ := ParseFloat(msg.Data.Ask1Size)

	return &exchanges.PriceUpdate{
		Exchange:  ExchangeName,
		Symbol:    msg.Data.Symbol,
		BestBid:   bestBid,
		BestAsk:   bestAsk,
		BidQty:    bidQty,
		AskQty:    askQty,
		Orderbook: nil, // Тикер не содержит полный стакан
		Timestamp: time.UnixMilli(msg.Timestamp),
	}, nil
}

// =============================================================================
// Парсинг ответов на ордера
// =============================================================================

// ParseOrderResponse преобразует OrderInfo от Bybit в общий exchanges.OrderResponse.
func ParseOrderResponse(info *OrderInfo) (*exchanges.OrderResponse, error) {
	if info == nil {
		return nil, fmt.Errorf("order info is nil")
	}

	// Парсим числовые поля
	filledQty, err := ParseFloat(info.CumExecQty)
	if err != nil {
		return nil, fmt.Errorf("failed to parse filled qty: %w", err)
	}

	avgPrice, err := ParseFloat(info.AvgPrice)
	if err != nil {
		return nil, fmt.Errorf("failed to parse avg price: %w", err)
	}

	// Парсим время создания
	createdMs, _ := ParseInt(info.CreatedTime)
	createdAt := time.UnixMilli(createdMs)

	return &exchanges.OrderResponse{
		OrderID:      info.OrderID,
		ClientID:     info.OrderLinkId,
		Symbol:       info.Symbol,
		Side:         convertOrderSide(info.Side),
		PositionSide: convertPositionSide(info.PositionIdx),
		FilledQty:    filledQty,
		AvgPrice:     avgPrice,
		Status:       convertOrderStatus(info.OrderStatus),
		CreatedAt:    createdAt,
		Exchange:     ExchangeName,
	}, nil
}

// ParsePlaceOrderResult преобразует результат создания ордера в OrderResponse.
// Используется сразу после PlaceOrder, когда известен только OrderID.
func ParsePlaceOrderResult(result *PlaceOrderResult, req *PlaceOrderRequest) *exchanges.OrderResponse {
	if result == nil {
		return nil
	}

	return &exchanges.OrderResponse{
		OrderID:      result.OrderID,
		ClientID:     result.OrderLinkId,
		Symbol:       req.Symbol,
		Side:         convertOrderSide(req.Side),
		PositionSide: convertPositionSide(req.PositionIdx),
		FilledQty:    0, // Пока неизвестно
		AvgPrice:     0, // Пока неизвестно
		Status:       exchanges.OrderStatusNew,
		CreatedAt:    time.Now(),
		Exchange:     ExchangeName,
	}
}

// =============================================================================
// Парсинг позиций
// =============================================================================

// ParsedPosition содержит распарсенные данные позиции для мониторинга.
type ParsedPosition struct {
	Symbol        string
	Side          exchanges.PositionSide
	Size          float64
	AvgPrice      float64
	UnrealizedPNL float64
	Leverage      int
	LiqPrice      float64
}

// ParsePosition преобразует PositionInfo в ParsedPosition.
func ParsePosition(info *PositionInfo) (*ParsedPosition, error) {
	if info == nil {
		return nil, fmt.Errorf("position info is nil")
	}

	size, err := ParseFloat(info.Size)
	if err != nil {
		return nil, fmt.Errorf("failed to parse size: %w", err)
	}

	// Пропускаем пустые позиции
	if size == 0 {
		return nil, nil
	}

	avgPrice, _ := ParseFloat(info.AvgPrice)
	unrealizedPNL, _ := ParseFloat(info.UnrealisedPnl)
	leverage, _ := ParseInt(info.Leverage)
	liqPrice, _ := ParseFloat(info.LiqPrice)

	var side exchanges.PositionSide
	if info.Side == SideBuy {
		side = exchanges.PositionSideLong
	} else {
		side = exchanges.PositionSideShort
	}

	return &ParsedPosition{
		Symbol:        info.Symbol,
		Side:          side,
		Size:          size,
		AvgPrice:      avgPrice,
		UnrealizedPNL: unrealizedPNL,
		Leverage:      int(leverage),
		LiqPrice:      liqPrice,
	}, nil
}

// =============================================================================
// Парсинг информации об инструменте
// =============================================================================

// ParseInstrumentLimits преобразует InstrumentInfo в exchanges.InstrumentLimits.
func ParseInstrumentLimits(info *InstrumentInfo) (*exchanges.InstrumentLimits, error) {
	if info == nil {
		return nil, fmt.Errorf("instrument info is nil")
	}

	minQty, err := ParseFloat(info.LotSizeFilter.MinOrderQty)
	if err != nil {
		return nil, fmt.Errorf("failed to parse min qty: %w", err)
	}

	maxQty, _ := ParseFloat(info.LotSizeFilter.MaxOrderQty)
	qtyStep, _ := ParseFloat(info.LotSizeFilter.QtyStep)
	tickSize, _ := ParseFloat(info.PriceFilter.TickSize)

	return &exchanges.InstrumentLimits{
		MinQty:   minQty,
		MaxQty:   maxQty,
		QtyStep:  qtyStep,
		TickSize: tickSize,
	}, nil
}

// =============================================================================
// Парсинг баланса
// =============================================================================

// ParseBalance преобразует CoinInfo в exchanges.Balance.
func ParseBalance(info *CoinInfo) (*exchanges.Balance, error) {
	if info == nil {
		return nil, fmt.Errorf("coin info is nil")
	}

	walletBalance, _ := ParseFloat(info.WalletBalance)
	availableToWithdraw, _ := ParseFloat(info.AvailableToWithdraw)
	equity, _ := ParseFloat(info.Equity)

	// В Bybit: availableToWithdraw ≈ свободная маржа
	// walletBalance - availableToWithdraw ≈ маржа в позициях
	inPosition := walletBalance - availableToWithdraw
	if inPosition < 0 {
		inPosition = 0
	}

	return &exchanges.Balance{
		Asset:      info.Coin,
		Available:  availableToWithdraw,
		InPosition: inPosition,
		Total:      equity,
	}, nil
}

// =============================================================================
// Конвертация типов
// =============================================================================

// convertOrderSide конвертирует направление ордера Bybit в общий тип.
func convertOrderSide(side string) exchanges.OrderSide {
	switch side {
	case SideBuy:
		return exchanges.OrderSideBuy
	case SideSell:
		return exchanges.OrderSideSell
	default:
		return exchanges.OrderSideBuy
	}
}

// convertPositionSide конвертирует индекс позиции Bybit в PositionSide.
// positionIdx: 0 = one-way mode, 1 = buy/long, 2 = sell/short
func convertPositionSide(positionIdx int) exchanges.PositionSide {
	switch positionIdx {
	case 1:
		return exchanges.PositionSideLong
	case 2:
		return exchanges.PositionSideShort
	default:
		return exchanges.PositionSideLong // One-way mode — по умолчанию long
	}
}

// convertOrderStatus конвертирует статус ордера Bybit в общий тип.
func convertOrderStatus(status string) exchanges.OrderStatus {
	switch status {
	case OrderStatusNew:
		return exchanges.OrderStatusNew
	case OrderStatusPartiallyFilled:
		return exchanges.OrderStatusPartiallyFilled
	case OrderStatusFilled:
		return exchanges.OrderStatusFilled
	case OrderStatusCancelled, OrderStatusDeactivated:
		return exchanges.OrderStatusCancelled
	case OrderStatusRejected:
		return exchanges.OrderStatusRejected
	default:
		return exchanges.OrderStatusNew
	}
}

// =============================================================================
// Конвертация в формат Bybit
// =============================================================================

// ToBybitSide конвертирует общий OrderSide в формат Bybit.
func ToBybitSide(side exchanges.OrderSide) string {
	switch side {
	case exchanges.OrderSideBuy:
		return SideBuy
	case exchanges.OrderSideSell:
		return SideSell
	default:
		return SideBuy
	}
}

// ToBybitOrderType конвертирует общий OrderType в формат Bybit.
func ToBybitOrderType(orderType exchanges.OrderType) string {
	switch orderType {
	case exchanges.OrderTypeMarket:
		return OrderTypeMarket
	case exchanges.OrderTypeLimit:
		return OrderTypeLimit
	default:
		return OrderTypeMarket
	}
}

// ToBybitPositionIdx конвертирует PositionSide в positionIdx для Bybit.
// Возвращает: 1 = long, 2 = short (hedge mode)
func ToBybitPositionIdx(side exchanges.PositionSide) int {
	switch side {
	case exchanges.PositionSideLong:
		return 1
	case exchanges.PositionSideShort:
		return 2
	default:
		return 1
	}
}

// =============================================================================
// Валидация сообщений
// =============================================================================

// ValidateOrderbookData проверяет корректность данных стакана.
func ValidateOrderbookData(data *WsOrderbookData) error {
	if data == nil {
		return fmt.Errorf("orderbook data is nil")
	}
	if data.Symbol == "" {
		return fmt.Errorf("symbol is empty")
	}
	if len(data.Bids) == 0 && len(data.Asks) == 0 {
		return fmt.Errorf("orderbook is empty")
	}
	return nil
}

// ValidatePriceUpdate проверяет корректность PriceUpdate.
func ValidatePriceUpdate(update *exchanges.PriceUpdate) error {
	if update == nil {
		return fmt.Errorf("price update is nil")
	}
	if update.Symbol == "" {
		return fmt.Errorf("symbol is empty")
	}
	if update.BestBid <= 0 || update.BestAsk <= 0 {
		return fmt.Errorf("invalid prices: bid=%f, ask=%f", update.BestBid, update.BestAsk)
	}
	if update.BestBid >= update.BestAsk {
		return fmt.Errorf("bid >= ask: %f >= %f", update.BestBid, update.BestAsk)
	}
	return nil
}
