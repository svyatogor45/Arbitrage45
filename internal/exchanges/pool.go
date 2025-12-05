package exchanges

import (
	"sync"
	"sync/atomic"
	"time"
)

// =====================================================================
// Пулы объектов для пакета exchanges
// =====================================================================
//
// Использование пулов снижает нагрузку на GC в hot path.
// Пулы размещены в том же пакете, что и типы, для избежания
// циклических импортов и дублирования типов.
// =====================================================================

// =====================================================================
// Статистика пулов (для мониторинга эффективности)
// =====================================================================

// PoolStats содержит статистику использования пулов.
type PoolStats struct {
	PriceUpdateHits   uint64 // Количество успешных получений из пула
	PriceUpdateMisses uint64 // Количество созданий новых объектов
	OrderBookHits     uint64 // Количество успешных получений из пула
	OrderBookMisses   uint64 // Количество созданий новых объектов
}

var poolStats PoolStats

// GetPoolStats возвращает статистику использования пулов.
func GetPoolStats() PoolStats {
	return PoolStats{
		PriceUpdateHits:   atomic.LoadUint64(&poolStats.PriceUpdateHits),
		PriceUpdateMisses: atomic.LoadUint64(&poolStats.PriceUpdateMisses),
		OrderBookHits:     atomic.LoadUint64(&poolStats.OrderBookHits),
		OrderBookMisses:   atomic.LoadUint64(&poolStats.OrderBookMisses),
	}
}

// ResetPoolStats сбрасывает статистику пулов.
func ResetPoolStats() {
	atomic.StoreUint64(&poolStats.PriceUpdateHits, 0)
	atomic.StoreUint64(&poolStats.PriceUpdateMisses, 0)
	atomic.StoreUint64(&poolStats.OrderBookHits, 0)
	atomic.StoreUint64(&poolStats.OrderBookMisses, 0)
}

// =====================================================================
// PriceUpdate Pool
// =====================================================================

// priceUpdatePool - пул для переиспользования объектов PriceUpdate.
var priceUpdatePool = sync.Pool{
	New: func() interface{} {
		atomic.AddUint64(&poolStats.PriceUpdateMisses, 1)
		return &PriceUpdate{}
	},
}

// GetPriceUpdate получает PriceUpdate из пула.
// Объект может содержать данные от предыдущего использования,
// поэтому все поля нужно установить перед использованием.
//
// Пример использования:
//
//	update := exchanges.GetPriceUpdate()
//	defer exchanges.PutPriceUpdate(update)
//	update.Exchange = "bybit"
//	update.Symbol = "BTCUSDT"
//	// ... заполнить остальные поля
func GetPriceUpdate() *PriceUpdate {
	obj := priceUpdatePool.Get()
	if obj != nil {
		atomic.AddUint64(&poolStats.PriceUpdateHits, 1)
	}
	return obj.(*PriceUpdate)
}

// PutPriceUpdate возвращает PriceUpdate в пул для повторного использования.
// Объект сбрасывается перед возвратом для предотвращения утечки данных.
//
// ВАЖНО: После вызова PutPriceUpdate не следует использовать объект!
func PutPriceUpdate(p *PriceUpdate) {
	if p == nil {
		return
	}

	// Сбросить все поля для предотвращения утечки данных
	p.Reset()

	priceUpdatePool.Put(p)
}

// PutPriceUpdateWithOrderBook возвращает PriceUpdate и его OrderBook в пулы.
// Используйте этот метод, если Orderbook также был получен из пула.
func PutPriceUpdateWithOrderBook(p *PriceUpdate) {
	if p == nil {
		return
	}

	// Сначала вернуть OrderBook в его пул
	if p.Orderbook != nil {
		PutOrderBook(p.Orderbook)
		p.Orderbook = nil
	}

	p.Reset()
	priceUpdatePool.Put(p)
}

// Reset сбрасывает все поля PriceUpdate в нулевые значения.
func (p *PriceUpdate) Reset() {
	p.Exchange = ""
	p.Symbol = ""
	p.BestBid = 0
	p.BestAsk = 0
	p.BidQty = 0
	p.AskQty = 0
	p.Orderbook = nil
	p.Timestamp = time.Time{}
}

// =====================================================================
// OrderBook Pool
// =====================================================================

// defaultOrderBookLevels - стандартная глубина стакана (из Requirements.md: 10 уровней)
const defaultOrderBookLevels = 10

// maxOrderBookLevels - максимальная глубина стакана для возврата в пул
// Если capacity превышена - объект не возвращается в пул для предотвращения утечки памяти
const maxOrderBookLevels = 20

// orderbookPool - пул для переиспользования объектов OrderBook.
var orderbookPool = sync.Pool{
	New: func() interface{} {
		atomic.AddUint64(&poolStats.OrderBookMisses, 1)
		return &OrderBook{
			// Предаллоцировать слайсы для стандартной глубины
			Bids: make([]Level, 0, defaultOrderBookLevels),
			Asks: make([]Level, 0, defaultOrderBookLevels),
		}
	},
}

// GetOrderBook получает OrderBook из пула.
// Слайсы Bids и Asks предаллоцированы на 10 уровней (стандартная глубина).
//
// Пример использования:
//
//	book := exchanges.GetOrderBook()
//	defer exchanges.PutOrderBook(book)
//	book.Bids = append(book.Bids, exchanges.Level{Price: 50000, Quantity: 0.5})
func GetOrderBook() *OrderBook {
	obj := orderbookPool.Get()
	if obj != nil {
		atomic.AddUint64(&poolStats.OrderBookHits, 1)
	}
	return obj.(*OrderBook)
}

// PutOrderBook возвращает OrderBook в пул для повторного использования.
// Слайсы очищаются, но capacity сохраняется для эффективного переиспользования.
//
// ВАЖНО:
//   - После вызова PutOrderBook не следует использовать объект!
//   - Если capacity слайсов слишком большая, объект не возвращается в пул
func PutOrderBook(book *OrderBook) {
	if book == nil {
		return
	}

	// Проверить, не вырос ли объект слишком большим
	// Это предотвращает утечку памяти при обработке аномально больших стаканов
	if cap(book.Bids) > maxOrderBookLevels*2 || cap(book.Asks) > maxOrderBookLevels*2 {
		// Не возвращаем в пул - пусть GC соберёт
		return
	}

	book.Reset()
	orderbookPool.Put(book)
}

// Reset сбрасывает OrderBook, сохраняя capacity слайсов.
func (book *OrderBook) Reset() {
	// Очищаем слайсы, сохраняя capacity
	book.Bids = book.Bids[:0]
	book.Asks = book.Asks[:0]
	book.UpdateID = 0
}

// =====================================================================
// Level Slice Pool
// =====================================================================

// levelSlicePool - пул для слайсов уровней стакана.
// Используется для временных операций с уровнями.
var levelSlicePool = sync.Pool{
	New: func() interface{} {
		slice := make([]Level, 0, defaultOrderBookLevels)
		return &slice
	},
}

// GetLevelSlice получает слайс Level из пула.
// Слайс предаллоцирован на 10 элементов.
func GetLevelSlice() *[]Level {
	return levelSlicePool.Get().(*[]Level)
}

// PutLevelSlice возвращает слайс Level в пул.
func PutLevelSlice(levels *[]Level) {
	if levels == nil {
		return
	}

	// Ограничить capacity
	if cap(*levels) > maxOrderBookLevels*2 {
		return
	}

	*levels = (*levels)[:0]
	levelSlicePool.Put(levels)
}
