package config

// TakerFees содержит комиссии taker для каждой биржи.
// Все значения в десятичных дробях (например, 0.00055 = 0.055%).
// Источник: документация бирж, стандартные ставки на perpetual futures.
var TakerFees = map[string]float64{
	"bybit":  0.00055, // 0.055%
	"bitget": 0.00060, // 0.060%
	"bingx":  0.00050, // 0.050%
	"gate":   0.00050, // 0.050%
	"okx":    0.00050, // 0.050%
	"htx":    0.00040, // 0.040% - наименьшая комиссия
	"mexc":   0.00040, // 0.040%
}

// GetTakerFee возвращает комиссию taker для указанной биржи.
// Если биржа не найдена, возвращает 0.0006 (0.06%) как значение по умолчанию.
func GetTakerFee(exchange string) float64 {
	if fee, exists := TakerFees[exchange]; exists {
		return fee
	}
	// Значение по умолчанию (консервативная оценка)
	return 0.0006
}

// CalculateNetSpread вычисляет чистый спред с учётом комиссий на вход и выход.
// Формула: Net_spread = Gross_spread - 2 × (fee_exchange_A + fee_exchange_B)
//
// Параметры:
//   - grossSpread: грубый спред в процентах (например, 1.2 для 1.2%)
//   - exchangeA: название первой биржи
//   - exchangeB: название второй биржи
//
// Возвращает чистый спред в процентах.
func CalculateNetSpread(grossSpread float64, exchangeA, exchangeB string) float64 {
	feeA := GetTakerFee(exchangeA)
	feeB := GetTakerFee(exchangeB)

	// Комиссии на вход и выход
	totalFeesPercent := 2 * (feeA + feeB) * 100

	return grossSpread - totalFeesPercent
}

// GetTotalFeePercent возвращает суммарные комиссии (вход + выход) в процентах.
func GetTotalFeePercent(exchangeA, exchangeB string) float64 {
	feeA := GetTakerFee(exchangeA)
	feeB := GetTakerFee(exchangeB)

	return 2 * (feeA + feeB) * 100
}
