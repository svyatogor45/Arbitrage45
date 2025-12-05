package orderbook

import (
	"fmt"

	"arbitrage-terminal/internal/exchanges"
)

// MaxOrderbookLevels определяет максимальное количество уровней стакана для расчёта.
// Согласно Requirements.md: "Для расчёта средней цены исполнения учитываем 10 уровней стакана."
const MaxOrderbookLevels = 10

// CalculateAvgPrice рассчитывает среднюю цену исполнения для заданного объёма.
// Проходит по уровням стакана и вычисляет средневзвешенную цену.
//
// Параметры:
//   - book: стакан ордеров (OrderBook)
//   - volume: требуемый объём для исполнения
//   - side: сторона (buy/sell)
//
// Возвращает:
//   - среднюю цену исполнения
//   - ошибку, если в стакане недостаточно ликвидности
//
// Пример:
// Стакан ask (продажа):
//   50000 USDT - 0.5 BTC
//   50010 USDT - 0.6 BTC
//   50020 USDT - 0.8 BTC
// Требуется купить 1.5 BTC
// Средняя цена = (0.5*50000 + 0.6*50010 + 0.4*50020) / 1.5 = 50010.67 USDT
func CalculateAvgPrice(book *exchanges.OrderBook, volume float64, side exchanges.OrderSide) (float64, error) {
	if book == nil {
		return 0, fmt.Errorf("стакан ордеров пуст (orderbook is nil)")
	}

	if volume <= 0 {
		return 0, fmt.Errorf("объём должен быть положительным, получено: %.8f", volume)
	}

	// Явная валидация стороны ордера для предотвращения некорректных расчётов
	// Согласно требованию: "обрабатывать все ошибки"
	if side != exchanges.OrderSideBuy && side != exchanges.OrderSideSell {
		return 0, fmt.Errorf("некорректная сторона ордера: '%s' (допустимые значения: 'buy' или 'sell')", side)
	}

	var levels []exchanges.Level

	// Выбрать уровни в зависимости от стороны
	if side == exchanges.OrderSideBuy {
		// Для покупки берём asks (самые дешёвые продавцы)
		levels = book.Asks
	} else {
		// Для продажи берём bids (самые дорогие покупатели)
		levels = book.Bids
	}

	if len(levels) == 0 {
		return 0, fmt.Errorf("стакан ордеров не содержит уровней для стороны '%s'", side)
	}

	// Ограничить количество уровней согласно Requirements.md (10 уровней)
	if len(levels) > MaxOrderbookLevels {
		levels = levels[:MaxOrderbookLevels]
	}

	var totalCost float64
	var filledVolume float64

	// Проход по уровням стакана (максимум 10)
	for _, level := range levels {
		if filledVolume >= volume {
			break
		}

		// Сколько ещё нужно заполнить
		remaining := volume - filledVolume

		// Сколько можем взять с этого уровня
		takeQty := min(remaining, level.Quantity)

		// Добавить к общей стоимости
		totalCost += takeQty * level.Price
		filledVolume += takeQty
	}

	// Проверка: хватило ли ликвидности
	if filledVolume < volume {
		return 0, fmt.Errorf(
			"недостаточно ликвидности в стакане: запрошено %.8f, доступно %.8f",
			volume,
			filledVolume,
		)
	}

	// Средняя цена = общая стоимость / объём
	avgPrice := totalCost / filledVolume

	return avgPrice, nil
}

// GetBestPrice возвращает лучшую цену из стакана для указанной стороны.
//
// Параметры:
//   - book: стакан ордеров
//   - side: сторона (buy/sell)
//
// Возвращает:
//   - лучшую цену (bid для sell, ask для buy)
//   - ошибку, если стакан пуст
func GetBestPrice(book *exchanges.OrderBook, side exchanges.OrderSide) (float64, error) {
	if book == nil {
		return 0, fmt.Errorf("стакан ордеров пуст (orderbook is nil)")
	}

	// Валидация стороны ордера
	if side != exchanges.OrderSideBuy && side != exchanges.OrderSideSell {
		return 0, fmt.Errorf("некорректная сторона ордера: '%s' (допустимые значения: 'buy' или 'sell')", side)
	}

	if side == exchanges.OrderSideBuy {
		// Для покупки нужна лучшая ask (самая дешёвая цена продажи)
		if len(book.Asks) == 0 {
			return 0, fmt.Errorf("в стакане ордеров отсутствуют предложения на продажу (asks)")
		}
		return book.Asks[0].Price, nil
	}

	// Для продажи нужна лучшая bid (самая дорогая цена покупки)
	if len(book.Bids) == 0 {
		return 0, fmt.Errorf("в стакане ордеров отсутствуют предложения на покупку (bids)")
	}
	return book.Bids[0].Price, nil
}

// GetSpread возвращает спред между лучшим bid и ask.
//
// Параметры:
//   - book: стакан ордеров
//
// Возвращает:
//   - абсолютный спред (ask - bid)
//   - относительный спред в процентах ((ask - bid) / bid * 100)
//   - ошибку, если стакан пуст
func GetSpread(book *exchanges.OrderBook) (float64, float64, error) {
	if book == nil {
		return 0, 0, fmt.Errorf("стакан ордеров пуст (orderbook is nil)")
	}

	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		return 0, 0, fmt.Errorf("стакан ордеров неполный (отсутствуют bids или asks)")
	}

	bestBid := book.Bids[0].Price
	bestAsk := book.Asks[0].Price

	// Абсолютный спред
	absSpread := bestAsk - bestBid

	// Относительный спред в процентах
	relSpread := (absSpread / bestBid) * 100

	return absSpread, relSpread, nil
}

// Примечание: используется встроенная функция min() из Go 1.21+
