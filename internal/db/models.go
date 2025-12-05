package db

import (
	"fmt"
	"time"
)

// Константы для валидации leverage
const (
	MinLeverage = 1   // Минимальное плечо
	MaxLeverage = 125 // Максимальное плечо (большинство бирж поддерживают до 125x)
)

// PairConfig представляет конфигурацию торговой пары.
// Хранится в таблице pairs и определяет параметры для арбитражной торговли.
type PairConfig struct {
	ID          int       `db:"id"`
	Symbol      string    `db:"symbol"`       // Символ пары (например, "BTCUSDT")
	Volume      float64   `db:"volume"`       // Объём для торговли (в монетах актива)
	EntrySpread float64   `db:"entry_spread"` // Порог входа (%)
	ExitSpread  float64   `db:"exit_spread"`  // Порог выхода (%)
	NumOrders   int       `db:"num_orders"`   // Количество частей для входа/выхода
	StopLoss    *float64  `db:"stop_loss"`    // Stop Loss в USDT (nullable)
	Leverage    int       `db:"leverage"`     // Плечо (1-125x)
	Status      string    `db:"status"`       // Статус: PAUSED, READY, ENTERING, POSITION_OPEN, EXITING, ERROR
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// Trade представляет историю арбитражной сделки.
// Сохраняется в таблице trades после закрытия позиции.
type Trade struct {
	ID             int        `db:"id"`
	PairID         int        `db:"pair_id"`          // Ссылка на pairs.id
	EntryTime      time.Time  `db:"entry_time"`       // Время открытия позиции
	ExitTime       *time.Time `db:"exit_time"`        // Время закрытия позиции (nullable)
	EntrySpread    float64    `db:"entry_spread"`     // Спред при входе (%)
	ExitSpread     *float64   `db:"exit_spread"`      // Спред при выходе (%, nullable)
	RealizedPNL    *float64   `db:"realized_pnl"`     // Реализованный PNL в USDT (nullable)
	ExchangeLong   string     `db:"exchange_long"`    // Биржа для long позиции
	ExchangeShort  string     `db:"exchange_short"`   // Биржа для short позиции
	Volume         float64    `db:"volume"`           // Общий объём сделки
	ClosedBy       *string    `db:"closed_by"`        // Причина закрытия: 'target', 'stop_loss', 'liquidation', 'manual'
	CreatedAt      time.Time  `db:"created_at"`
}

// LeverageCache представляет кэш установленного плеча на бирже для символа.
// Используется для избежания повторных запросов к API биржи.
type LeverageCache struct {
	Exchange  string    `db:"exchange"`   // Название биржи (bybit, bitget и т.д.)
	Symbol    string    `db:"symbol"`     // Символ пары
	Leverage  int       `db:"leverage"`   // Установленное плечо
	UpdatedAt time.Time `db:"updated_at"` // Время последнего обновления
}

// PairStatus представляет возможные статусы торговой пары.
type PairStatus string

const (
	PairStatusPaused       PairStatus = "PAUSED"        // Пара на паузе (не торгуется)
	PairStatusReady        PairStatus = "READY"         // Мониторинг активен, ожидание условий входа
	PairStatusEntering     PairStatus = "ENTERING"      // Процесс входа в позицию
	PairStatusPositionOpen PairStatus = "POSITION_OPEN" // Позиция открыта, сопровождение
	PairStatusExiting      PairStatus = "EXITING"       // Процесс выхода из позиции
	PairStatusError        PairStatus = "ERROR"         // Ошибка, требуется вмешательство
)

// CloseReason представляет причину закрытия позиции.
type CloseReason string

const (
	CloseReasonTarget      CloseReason = "target"      // Достигнут целевой спред выхода
	CloseReasonStopLoss    CloseReason = "stop_loss"   // Сработал Stop Loss
	CloseReasonLiquidation CloseReason = "liquidation" // Ликвидация биржей
	CloseReasonManual      CloseReason = "manual"      // Ручное закрытие пользователем
)

// Validate проверяет корректность конфигурации пары.
// Возвращает ошибку, если какое-либо поле содержит недопустимое значение.
func (p *PairConfig) Validate() error {
	if p.Symbol == "" {
		return fmt.Errorf("symbol не может быть пустым")
	}

	if p.Volume <= 0 {
		return fmt.Errorf("volume должен быть положительным")
	}

	if p.EntrySpread <= 0 {
		return fmt.Errorf("entry_spread должен быть положительным")
	}

	if p.ExitSpread < 0 {
		return fmt.Errorf("exit_spread не может быть отрицательным")
	}

	// Кросс-валидация: exit_spread должен быть меньше entry_spread
	// Иначе арбитраж не имеет смысла (невозможно заработать)
	if p.ExitSpread >= p.EntrySpread {
		return fmt.Errorf("exit_spread (%.4f) должен быть меньше entry_spread (%.4f)", p.ExitSpread, p.EntrySpread)
	}

	if p.NumOrders < 1 {
		return fmt.Errorf("num_orders должен быть >= 1")
	}

	// Валидация StopLoss (если задан)
	if p.StopLoss != nil && *p.StopLoss <= 0 {
		return fmt.Errorf("stop_loss должен быть положительным, получено: %.2f", *p.StopLoss)
	}

	if p.Leverage < MinLeverage || p.Leverage > MaxLeverage {
		return fmt.Errorf("leverage должен быть в диапазоне %d-%d, получено: %d", MinLeverage, MaxLeverage, p.Leverage)
	}

	// Проверка статуса
	validStatuses := map[string]bool{
		string(PairStatusPaused):       true,
		string(PairStatusReady):        true,
		string(PairStatusEntering):     true,
		string(PairStatusPositionOpen): true,
		string(PairStatusExiting):      true,
		string(PairStatusError):        true,
	}
	if !validStatuses[p.Status] {
		return fmt.Errorf("недопустимый статус: %s", p.Status)
	}

	return nil
}
