package pool

import (
	"sync"
)

// =====================================================================
// Утилитарные пулы для переиспользования памяти
// =====================================================================
//
// ВАЖНО: Специфичные пулы для типов данных размещены в соответствующих пакетах:
//   - PriceUpdate, OrderBook → internal/exchanges/pool.go
//   - BestPrices → internal/core/prices/pool.go
//
// Это сделано для избежания циклических импортов и дублирования типов.
// =====================================================================

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
// Буфер очищается, но capacity сохраняется (если не превышает лимит).
func PutBytesBuffer(buf *BytesBuffer) {
	if buf == nil {
		return
	}

	// Ограничить capacity для предотвращения утечки памяти
	// Если буфер вырос слишком большим - не возвращаем в пул
	const maxCapacity = 64 * 1024 // 64KB
	if cap(buf.Bytes) > maxCapacity {
		return // Пусть GC соберёт большой буфер
	}

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

// =====================================================================
// Float64 Slice Pool
// =====================================================================

// float64SlicePool - пул для слайсов float64.
// Используется для временных вычислений.
var float64SlicePool = sync.Pool{
	New: func() interface{} {
		slice := make([]float64, 0, 32)
		return &slice
	},
}

// GetFloat64Slice получает слайс float64 из пула.
// Слайс предаллоцирован на 32 элемента.
func GetFloat64Slice() *[]float64 {
	return float64SlicePool.Get().(*[]float64)
}

// PutFloat64Slice возвращает слайс float64 в пул.
func PutFloat64Slice(slice *[]float64) {
	if slice == nil {
		return
	}

	// Ограничить capacity
	const maxCapacity = 256
	if cap(*slice) > maxCapacity {
		return
	}

	*slice = (*slice)[:0]
	float64SlicePool.Put(slice)
}

// =====================================================================
// String Slice Pool
// =====================================================================

// stringSlicePool - пул для слайсов строк.
// Используется для временных операций со списками символов/бирж.
var stringSlicePool = sync.Pool{
	New: func() interface{} {
		slice := make([]string, 0, 16)
		return &slice
	},
}

// GetStringSlice получает слайс строк из пула.
// Слайс предаллоцирован на 16 элементов.
func GetStringSlice() *[]string {
	return stringSlicePool.Get().(*[]string)
}

// PutStringSlice возвращает слайс строк в пул.
func PutStringSlice(slice *[]string) {
	if slice == nil {
		return
	}

	// Ограничить capacity
	const maxCapacity = 128
	if cap(*slice) > maxCapacity {
		return
	}

	// Обнулить ссылки для GC
	for i := range *slice {
		(*slice)[i] = ""
	}
	*slice = (*slice)[:0]
	stringSlicePool.Put(slice)
}

// =====================================================================
// Generic Map Pool (для map[string]interface{})
// =====================================================================

// mapPool - пул для map[string]interface{}.
// Используется для парсинга JSON и временных структур.
var mapPool = sync.Pool{
	New: func() interface{} {
		return make(map[string]interface{}, 16)
	},
}

// GetMap получает map[string]interface{} из пула.
func GetMap() map[string]interface{} {
	return mapPool.Get().(map[string]interface{})
}

// PutMap возвращает map в пул.
// Map очищается перед возвратом.
func PutMap(m map[string]interface{}) {
	if m == nil {
		return
	}

	// Ограничить размер
	const maxSize = 64
	if len(m) > maxSize {
		return
	}

	// Очистить map
	for k := range m {
		delete(m, k)
	}
	mapPool.Put(m)
}
