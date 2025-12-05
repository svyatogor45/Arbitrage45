package prices

import (
	"fmt"
	"sync"
	"time"

	"arbitrage-terminal/internal/config"
	"arbitrage-terminal/internal/exchanges"
)

// ExchangePrice содержит данные о ценах на одной бирже.
type ExchangePrice struct {
	BestBid   float64               // Лучшая цена покупки
	BestAsk   float64               // Лучшая цена продажи
	Orderbook *exchanges.OrderBook  // Полный стакан (опционально)
	UpdatedAt time.Time             // Время последнего обновления
}

// BestPrices содержит лучшие цены среди всех бирж.
type BestPrices struct {
	CheapestExchange string    // Биржа с самым дешёвым ask (для long)
	CheapestAsk      float64   // Самая дешёвая цена покупки
	DearestExchange  string    // Биржа с самым дорогим bid (для short)
	DearestBid       float64   // Самая дорогая цена продажи
	NetSpread        float64   // Чистый спред с учётом комиссий
	RawSpread        float64   // Сырой спред без комиссий
	Timestamp        time.Time // Время расчёта
}

// Tracker отслеживает цены для одного символа на всех биржах.
// Потокобезопасен (thread-safe).
type Tracker struct {
	symbol    string                       // Символ торговой пары (например, "BTCUSDT")
	prices    map[string]*ExchangePrice    // exchange name -> price data
	mu        sync.RWMutex                 // Мутекс для защиты prices
}

// NewTracker создаёт новый трекер цен для указанного символа.
func NewTracker(symbol string) *Tracker {
	return &Tracker{
		symbol: symbol,
		prices: make(map[string]*ExchangePrice),
	}
}

// Update обновляет цены от указанной биржи.
// Потокобезопасен - может вызываться из разных горутин.
func (t *Tracker) Update(exchange string, price *ExchangePrice) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.prices[exchange] = price
}

// GetBestPrices возвращает лучшие цены среди всех бирж с учётом комиссий.
// Находит самую дешёвую биржу для long и самую дорогую для short.
func (t *Tracker) GetBestPrices() *BestPrices {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if len(t.prices) == 0 {
		return nil
	}

	var cheapestExchange, dearestExchange string
	var cheapestAsk, dearestBid float64

	// Найти самую дешёвую ask (для открытия long)
	first := true
	for exchName, price := range t.prices {
		if price.BestAsk == 0 {
			continue
		}

		if first || price.BestAsk < cheapestAsk {
			cheapestAsk = price.BestAsk
			cheapestExchange = exchName
			first = false
		}
	}

	// Найти самую дорогую bid (для открытия short)
	first = true
	for exchName, price := range t.prices {
		if price.BestBid == 0 {
			continue
		}

		if first || price.BestBid > dearestBid {
			dearestBid = price.BestBid
			dearestExchange = exchName
			first = false
		}
	}

	// Проверка: если не нашли подходящие биржи
	if cheapestExchange == "" || dearestExchange == "" {
		return nil
	}

	// Сырой спред (без комиссий)
	rawSpread := dearestBid - cheapestAsk

	// Чистый спред с учётом комиссий
	// Формула: spread - 2 * (feeA + feeB)
	// Где feeA = комиссия на long бирже, feeB = комиссия на short бирже
	totalFeePercent := config.GetTotalFeePercent(cheapestExchange, dearestExchange)

	// Вычитаем комиссии из спреда
	netSpread := rawSpread - (cheapestAsk * totalFeePercent / 100)

	return &BestPrices{
		CheapestExchange: cheapestExchange,
		CheapestAsk:      cheapestAsk,
		DearestExchange:  dearestExchange,
		DearestBid:       dearestBid,
		NetSpread:        netSpread,
		RawSpread:        rawSpread,
		Timestamp:        time.Now(),
	}
}

// GetPrice возвращает цену для указанной биржи и стороны.
//
// Параметры:
//   - exchange: название биржи
//   - side: сторона (buy -> ask, sell -> bid)
//
// Возвращает цену или 0, если данных нет.
func (t *Tracker) GetPrice(exchange string, side exchanges.OrderSide) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	price, exists := t.prices[exchange]
	if !exists {
		return 0
	}

	if side == exchanges.OrderSideBuy {
		return price.BestAsk
	}
	return price.BestBid
}

// GetExchangePrice возвращает полную информацию о ценах на указанной бирже.
func (t *Tracker) GetExchangePrice(exchange string) (*ExchangePrice, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	price, exists := t.prices[exchange]
	if !exists {
		return nil, fmt.Errorf("no price data for exchange %s", exchange)
	}

	return price, nil
}

// GetSymbol возвращает символ торговой пары.
func (t *Tracker) GetSymbol() string {
	return t.symbol
}

// GetExchanges возвращает список бирж, для которых есть данные.
func (t *Tracker) GetExchanges() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	exchanges := make([]string, 0, len(t.prices))
	for exchName := range t.prices {
		exchanges = append(exchanges, exchName)
	}

	return exchanges
}

// IsStale проверяет, устарели ли данные для указанной биржи.
// Данные считаются устаревшими, если не обновлялись более maxAge.
func (t *Tracker) IsStale(exchange string, maxAge time.Duration) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	price, exists := t.prices[exchange]
	if !exists {
		return true
	}

	return time.Since(price.UpdatedAt) > maxAge
}

// Clear очищает все данные о ценах.
func (t *Tracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.prices = make(map[string]*ExchangePrice)
}
