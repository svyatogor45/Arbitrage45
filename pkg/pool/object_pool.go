package pool

import (
	"sync"
	"time"
)

// =====================================================================
// PriceUpdate Pool
// =====================================================================

// PriceUpdate представляет обновление цены из WebSocket.
// Это копия типа из internal/exchanges/common.go для избежания циклических импортов.
// При получении объекта из пула нужно заполнить все поля, при возврате - сбросить.
type PriceUpdate struct {
	Exchange  string        // Название биржи
	Symbol    string        // Символ торговой пары
	BestBid   float64       // Лучшая цена покупки
	BestAsk   float64       // Лучшая цена продажи
	BidQty    float64       // Объём на лучшем bid
	AskQty    float64       // Объём на лучшем ask
	Orderbook *OrderBook    // Полный стакан (опционально)
	Timestamp time.Time     // Время обновления
}

// OrderBook представляет стакан ордеров.
// Копия из internal/exchanges/common.go для избежания циклических импортов.
type OrderBook struct {
	Bids     []Level // Уровни покупки (отсортированы по убыванию цены)
	Asks     []Level // Уровни продажи (отсортированы по возрастанию цены)
	UpdateID int64   // ID обновления
}

// Level представляет один уровень в стакане.
type Level struct {
	Price    float64 // Цена
	Quantity float64 // Объём на этом уровне
}

// priceUpdatePool - пул для переиспользования объектов PriceUpdate.
// Использует sync.Pool для эффективного повторного использования памяти.
var priceUpdatePool = sync.Pool{
	New: func() interface{} {
		return &PriceUpdate{}
	},
}

// GetPriceUpdate получает PriceUpdate из пула.
// Объект может содержать данные от предыдущего использования,
// поэтому все поля нужно установить перед использованием.
//
// Пример использования:
//
//	update := pool.GetPriceUpdate()
//	defer pool.PutPriceUpdate(update)
//	update.Exchange = "bybit"
//	update.Symbol = "BTCUSDT"
//	// ... заполнить остальные поля
func GetPriceUpdate() *PriceUpdate {
	return priceUpdatePool.Get().(*PriceUpdate)
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

// Reset сбрасывает все поля PriceUpdate в нулевые значения.
// Используется перед возвратом объекта в пул.
func (p *PriceUpdate) Reset() {
	p.Exchange = ""
	p.Symbol = ""
	p.BestBid = 0
	p.BestAsk = 0
	p.BidQty = 0
	p.AskQty = 0
	p.Orderbook = nil // Не сбрасываем содержимое OrderBook - он тоже может быть из пула
	p.Timestamp = time.Time{}
}

// =====================================================================
// BestPrices Pool
// =====================================================================

// BestPrices содержит лучшие цены среди всех бирж.
// Копия из internal/core/prices/tracker.go для избежания циклических импортов.
type BestPrices struct {
	CheapestExchange string    // Биржа с самым дешёвым ask (для long)
	CheapestAsk      float64   // Самая дешёвая цена покупки
	DearestExchange  string    // Биржа с самым дорогим bid (для short)
	DearestBid       float64   // Самая дорогая цена продажи
	NetSpread        float64   // Чистый спред с учётом комиссий (в %)
	RawSpread        float64   // Сырой спред без комиссий (в %)
	AbsoluteSpread   float64   // Абсолютный спред в USDT
	Timestamp        time.Time // Время расчёта
}

// bestPricesPool - пул для переиспользования объектов BestPrices.
var bestPricesPool = sync.Pool{
	New: func() interface{} {
		return &BestPrices{}
	},
}

// GetBestPrices получает BestPrices из пула.
// Объект может содержать данные от предыдущего использования,
// поэтому все поля нужно установить перед использованием.
//
// Пример использования:
//
//	bp := pool.GetBestPrices()
//	defer pool.PutBestPrices(bp)
//	bp.CheapestExchange = "bybit"
//	// ... заполнить остальные поля
func GetBestPrices() *BestPrices {
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

	// Сбросить все поля
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
// OrderBook Pool
// =====================================================================

// orderbookPool - пул для переиспользования объектов OrderBook.
var orderbookPool = sync.Pool{
	New: func() interface{} {
		return &OrderBook{
			// Предаллоцировать слайсы для 10 уровней (стандартная глубина)
			Bids: make([]Level, 0, 10),
			Asks: make([]Level, 0, 10),
		}
	},
}

// GetOrderBook получает OrderBook из пула.
// Слайсы Bids и Asks предаллоцированы на 10 уровней (стандартная глубина).
//
// Пример использования:
//
//	book := pool.GetOrderBook()
//	defer pool.PutOrderBook(book)
//	book.Bids = append(book.Bids, pool.Level{Price: 50000, Quantity: 0.5})
//	// ... заполнить остальные поля
func GetOrderBook() *OrderBook {
	return orderbookPool.Get().(*OrderBook)
}

// PutOrderBook возвращает OrderBook в пул для повторного использования.
// Слайсы очищаются, но capacity сохраняется для эффективного переиспользования.
//
// ВАЖНО: После вызова PutOrderBook не следует использовать объект!
func PutOrderBook(book *OrderBook) {
	if book == nil {
		return
	}

	// Сбросить данные, сохранив capacity слайсов
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
		// Предаллоцировать слайс на 10 уровней
		slice := make([]Level, 0, 10)
		return &slice
	},
}

// GetLevelSlice получает слайс Level из пула.
// Слайс предаллоцирован на 10 элементов.
//
// Пример использования:
//
//	levels := pool.GetLevelSlice()
//	defer pool.PutLevelSlice(levels)
//	*levels = append(*levels, pool.Level{Price: 50000, Quantity: 0.5})
func GetLevelSlice() *[]Level {
	return levelSlicePool.Get().(*[]Level)
}

// PutLevelSlice возвращает слайс Level в пул.
// Слайс очищается, но capacity сохраняется.
func PutLevelSlice(levels *[]Level) {
	if levels == nil {
		return
	}

	// Очистить слайс, сохранив capacity
	*levels = (*levels)[:0]

	levelSlicePool.Put(levels)
}

// =====================================================================
// Bytes Buffer Pool
// =====================================================================

// BytesBuffer представляет переиспользуемый буфер байтов.
// Используется для парсинга JSON и других операций с байтами.
type BytesBuffer struct {
	Bytes []byte
}

// bytesBufferPool - пул для буферов байтов.
// Начальный размер буфера - 4KB, что достаточно для большинства WebSocket сообщений.
var bytesBufferPool = sync.Pool{
	New: func() interface{} {
		return &BytesBuffer{
			Bytes: make([]byte, 0, 4096), // 4KB начальный размер
		}
	},
}

// GetBytesBuffer получает BytesBuffer из пула.
// Буфер предаллоцирован на 4KB.
//
// Пример использования:
//
//	buf := pool.GetBytesBuffer()
//	defer pool.PutBytesBuffer(buf)
//	buf.Bytes = append(buf.Bytes, data...)
func GetBytesBuffer() *BytesBuffer {
	return bytesBufferPool.Get().(*BytesBuffer)
}

// PutBytesBuffer возвращает BytesBuffer в пул.
// Буфер очищается, но capacity сохраняется.
func PutBytesBuffer(buf *BytesBuffer) {
	if buf == nil {
		return
	}

	// Очистить буфер, сохранив capacity
	buf.Bytes = buf.Bytes[:0]

	bytesBufferPool.Put(buf)
}

// Reset сбрасывает буфер, сохраняя capacity.
func (buf *BytesBuffer) Reset() {
	buf.Bytes = buf.Bytes[:0]
}

// Write добавляет данные в буфер (реализует io.Writer).
func (buf *BytesBuffer) Write(data []byte) (int, error) {
	buf.Bytes = append(buf.Bytes, data...)
	return len(data), nil
}

// String возвращает содержимое буфера как строку.
func (buf *BytesBuffer) String() string {
	return string(buf.Bytes)
}

// Len возвращает текущую длину буфера.
func (buf *BytesBuffer) Len() int {
	return len(buf.Bytes)
}

// Cap возвращает capacity буфера.
func (buf *BytesBuffer) Cap() int {
	return cap(buf.Bytes)
}
