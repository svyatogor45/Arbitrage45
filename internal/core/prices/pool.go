package prices

import (
	"sync"
	"sync/atomic"
	"time"
)

// =====================================================================
// Пул объектов для пакета prices
// =====================================================================
//
// Использование пула снижает нагрузку на GC в hot path.
// Пул размещён в том же пакете, что и тип BestPrices,
// для избежания циклических импортов и дублирования типов.
// =====================================================================

// =====================================================================
// Статистика пула (для мониторинга эффективности)
// =====================================================================

var (
	bestPricesHits   uint64 // Количество успешных получений из пула
	bestPricesMisses uint64 // Количество созданий новых объектов
)

// GetBestPricesPoolStats возвращает статистику использования пула BestPrices.
func GetBestPricesPoolStats() (hits, misses uint64) {
	return atomic.LoadUint64(&bestPricesHits), atomic.LoadUint64(&bestPricesMisses)
}

// ResetBestPricesPoolStats сбрасывает статистику пула.
func ResetBestPricesPoolStats() {
	atomic.StoreUint64(&bestPricesHits, 0)
	atomic.StoreUint64(&bestPricesMisses, 0)
}

// =====================================================================
// BestPrices Pool
// =====================================================================

// bestPricesPool - пул для переиспользования объектов BestPrices.
var bestPricesPool = sync.Pool{
	New: func() interface{} {
		atomic.AddUint64(&bestPricesMisses, 1)
		return &BestPrices{}
	},
}

// GetBestPrices получает BestPrices из пула.
// Объект может содержать данные от предыдущего использования,
// поэтому все поля нужно установить перед использованием.
//
// Пример использования:
//
//	bp := prices.GetBestPricesFromPool()
//	defer prices.PutBestPrices(bp)
//	bp.CheapestExchange = "bybit"
//	// ... заполнить остальные поля
func GetBestPricesFromPool() *BestPrices {
	obj := bestPricesPool.Get()
	if obj != nil {
		atomic.AddUint64(&bestPricesHits, 1)
	}
	return obj.(*BestPrices)
}

// PutBestPrices возвращает BestPrices в пул для повторного использования.
// Объект сбрасывается перед возвратом для предотвращения утечки данных.
//
// ВАЖНО: После вызова PutBestPrices не следует использовать объект!
func PutBestPrices(bp *BestPrices) {
	if bp == nil {
		return
	}

	bp.Reset()
	bestPricesPool.Put(bp)
}

// Reset сбрасывает все поля BestPrices в нулевые значения.
func (bp *BestPrices) Reset() {
	bp.CheapestExchange = ""
	bp.CheapestAsk = 0
	bp.DearestExchange = ""
	bp.DearestBid = 0
	bp.NetSpread = 0
	bp.RawSpread = 0
	bp.AbsoluteSpread = 0
	bp.Timestamp = time.Time{}
}

// =====================================================================
// ExchangePrice Pool
// =====================================================================

var (
	exchangePriceHits   uint64
	exchangePriceMisses uint64
)

// GetExchangePricePoolStats возвращает статистику использования пула ExchangePrice.
func GetExchangePricePoolStats() (hits, misses uint64) {
	return atomic.LoadUint64(&exchangePriceHits), atomic.LoadUint64(&exchangePriceMisses)
}

// exchangePricePool - пул для переиспользования объектов ExchangePrice.
var exchangePricePool = sync.Pool{
	New: func() interface{} {
		atomic.AddUint64(&exchangePriceMisses, 1)
		return &ExchangePrice{}
	},
}

// GetExchangePriceFromPool получает ExchangePrice из пула.
func GetExchangePriceFromPool() *ExchangePrice {
	obj := exchangePricePool.Get()
	if obj != nil {
		atomic.AddUint64(&exchangePriceHits, 1)
	}
	return obj.(*ExchangePrice)
}

// PutExchangePrice возвращает ExchangePrice в пул.
func PutExchangePrice(ep *ExchangePrice) {
	if ep == nil {
		return
	}

	ep.Reset()
	exchangePricePool.Put(ep)
}

// Reset сбрасывает все поля ExchangePrice.
func (ep *ExchangePrice) Reset() {
	ep.BestBid = 0
	ep.BestAsk = 0
	ep.Orderbook = nil
	ep.UpdatedAt = time.Time{}
}
