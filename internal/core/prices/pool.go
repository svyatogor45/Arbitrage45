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
	bestPricesGets    uint64 // Общее количество запросов Get()
	bestPricesCreates uint64 // Количество созданий новых объектов (miss)

	exchangePriceGets    uint64 // Общее количество запросов Get()
	exchangePriceCreates uint64 // Количество созданий новых объектов (miss)
)

// GetBestPricesPoolStats возвращает статистику использования пула BestPrices.
// Возвращает: gets (общее количество запросов), creates (количество созданий новых объектов).
// Hit rate = (gets - creates) / gets * 100
func GetBestPricesPoolStats() (gets, creates uint64) {
	return atomic.LoadUint64(&bestPricesGets), atomic.LoadUint64(&bestPricesCreates)
}

// BestPricesHitRate возвращает процент успешных получений из кэша.
// Возвращает значение от 0 до 100.
func BestPricesHitRate() float64 {
	gets := atomic.LoadUint64(&bestPricesGets)
	if gets == 0 {
		return 0
	}
	creates := atomic.LoadUint64(&bestPricesCreates)
	if creates > gets {
		return 0 // Защита от переполнения
	}
	return float64(gets-creates) / float64(gets) * 100
}

// ResetBestPricesPoolStats сбрасывает статистику пула.
func ResetBestPricesPoolStats() {
	atomic.StoreUint64(&bestPricesGets, 0)
	atomic.StoreUint64(&bestPricesCreates, 0)
}

// =====================================================================
// BestPrices Pool
// =====================================================================

// bestPricesPool - пул для переиспользования объектов BestPrices.
var bestPricesPool = sync.Pool{
	New: func() interface{} {
		// Увеличиваем счётчик создания новых объектов (cache miss)
		atomic.AddUint64(&bestPricesCreates, 1)
		return &BestPrices{}
	},
}

// GetBestPricesFromPool получает BestPrices из пула.
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
	// Увеличиваем счётчик запросов
	atomic.AddUint64(&bestPricesGets, 1)
	return bestPricesPool.Get().(*BestPrices)
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

// GetExchangePricePoolStats возвращает статистику использования пула ExchangePrice.
// Возвращает: gets (общее количество запросов), creates (количество созданий новых объектов).
func GetExchangePricePoolStats() (gets, creates uint64) {
	return atomic.LoadUint64(&exchangePriceGets), atomic.LoadUint64(&exchangePriceCreates)
}

// ExchangePriceHitRate возвращает процент успешных получений из кэша.
// Возвращает значение от 0 до 100.
func ExchangePriceHitRate() float64 {
	gets := atomic.LoadUint64(&exchangePriceGets)
	if gets == 0 {
		return 0
	}
	creates := atomic.LoadUint64(&exchangePriceCreates)
	if creates > gets {
		return 0
	}
	return float64(gets-creates) / float64(gets) * 100
}

// ResetExchangePricePoolStats сбрасывает статистику пула.
func ResetExchangePricePoolStats() {
	atomic.StoreUint64(&exchangePriceGets, 0)
	atomic.StoreUint64(&exchangePriceCreates, 0)
}

// exchangePricePool - пул для переиспользования объектов ExchangePrice.
var exchangePricePool = sync.Pool{
	New: func() interface{} {
		// Увеличиваем счётчик создания новых объектов (cache miss)
		atomic.AddUint64(&exchangePriceCreates, 1)
		return &ExchangePrice{}
	},
}

// GetExchangePriceFromPool получает ExchangePrice из пула.
func GetExchangePriceFromPool() *ExchangePrice {
	// Увеличиваем счётчик запросов
	atomic.AddUint64(&exchangePriceGets, 1)
	return exchangePricePool.Get().(*ExchangePrice)
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
