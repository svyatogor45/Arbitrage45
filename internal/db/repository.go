package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // PostgreSQL драйвер
)

// Repository предоставляет методы для работы с базой данных.
type Repository struct {
	db *sql.DB
}

// NewRepository создаёт новый экземпляр Repository.
// Параметры:
//   - dsn: строка подключения к PostgreSQL (из DatabaseConfig.GetDSN())
//   - maxConnections: максимальное количество соединений в пуле (из конфигурации)
func NewRepository(dsn string, maxConnections int) (*Repository, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть соединение с БД: %w", err)
	}

	// Настройка пула соединений
	if maxConnections > 0 {
		db.SetMaxOpenConns(maxConnections)
		db.SetMaxIdleConns(maxConnections / 2) // Половина от максимума
	} else {
		db.SetMaxOpenConns(20)  // Значение по умолчанию
		db.SetMaxIdleConns(10)
	}
	db.SetConnMaxLifetime(5 * time.Minute) // Максимальное время жизни соединения

	// Проверка соединения
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("не удалось подключиться к БД: %w", err)
	}

	return &Repository{db: db}, nil
}

// Close закрывает соединение с базой данных.
func (r *Repository) Close() error {
	return r.db.Close()
}

// === Операции с парами (pairs) ===

// CreatePair создаёт новую конфигурацию торговой пары.
// Возвращает ID созданной записи.
// Перед вставкой выполняется валидация данных.
func (r *Repository) CreatePair(pair *PairConfig) (int, error) {
	// Валидация перед вставкой
	if err := pair.Validate(); err != nil {
		return 0, fmt.Errorf("ошибка валидации пары: %w", err)
	}

	query := `
		INSERT INTO pairs (symbol, volume, entry_spread, exit_spread, num_orders, stop_loss, leverage, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`

	var id int
	err := r.db.QueryRow(
		query,
		pair.Symbol,
		pair.Volume,
		pair.EntrySpread,
		pair.ExitSpread,
		pair.NumOrders,
		pair.StopLoss,
		pair.Leverage,
		pair.Status,
	).Scan(&id)

	if err != nil {
		return 0, fmt.Errorf("не удалось создать пару: %w", err)
	}

	return id, nil
}

// GetPair получает конфигурацию пары по ID.
func (r *Repository) GetPair(id int) (*PairConfig, error) {
	query := `
		SELECT id, symbol, volume, entry_spread, exit_spread, num_orders,
		       stop_loss, leverage, status, created_at, updated_at
		FROM pairs
		WHERE id = $1
	`

	var pair PairConfig
	err := r.db.QueryRow(query, id).Scan(
		&pair.ID,
		&pair.Symbol,
		&pair.Volume,
		&pair.EntrySpread,
		&pair.ExitSpread,
		&pair.NumOrders,
		&pair.StopLoss,
		&pair.Leverage,
		&pair.Status,
		&pair.CreatedAt,
		&pair.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("пара с ID %d не найдена", id)
	}
	if err != nil {
		return nil, fmt.Errorf("ошибка при получении пары: %w", err)
	}

	return &pair, nil
}

// GetAllPairs получает все торговые пары.
func (r *Repository) GetAllPairs() ([]*PairConfig, error) {
	query := `
		SELECT id, symbol, volume, entry_spread, exit_spread, num_orders,
		       stop_loss, leverage, status, created_at, updated_at
		FROM pairs
		ORDER BY created_at DESC
	`

	rows, err := r.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить пары: %w", err)
	}
	defer rows.Close()

	var pairs []*PairConfig
	for rows.Next() {
		var pair PairConfig
		err := rows.Scan(
			&pair.ID,
			&pair.Symbol,
			&pair.Volume,
			&pair.EntrySpread,
			&pair.ExitSpread,
			&pair.NumOrders,
			&pair.StopLoss,
			&pair.Leverage,
			&pair.Status,
			&pair.CreatedAt,
			&pair.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("ошибка при сканировании пары: %w", err)
		}
		pairs = append(pairs, &pair)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка при итерации по парам: %w", err)
	}

	return pairs, nil
}

// UpdatePair обновляет конфигурацию пары.
// Перед обновлением выполняется валидация данных.
func (r *Repository) UpdatePair(pair *PairConfig) error {
	// Валидация перед обновлением
	if err := pair.Validate(); err != nil {
		return fmt.Errorf("ошибка валидации пары: %w", err)
	}

	query := `
		UPDATE pairs
		SET symbol = $2, volume = $3, entry_spread = $4, exit_spread = $5,
		    num_orders = $6, stop_loss = $7, leverage = $8, status = $9,
		    updated_at = NOW()
		WHERE id = $1
	`

	result, err := r.db.Exec(
		query,
		pair.ID,
		pair.Symbol,
		pair.Volume,
		pair.EntrySpread,
		pair.ExitSpread,
		pair.NumOrders,
		pair.StopLoss,
		pair.Leverage,
		pair.Status,
	)

	if err != nil {
		return fmt.Errorf("не удалось обновить пару: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("не удалось получить количество обновлённых строк: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("пара с ID %d не найдена", pair.ID)
	}

	return nil
}

// DeletePair удаляет пару по ID.
// Также удаляет все связанные сделки (CASCADE).
func (r *Repository) DeletePair(id int) error {
	query := `DELETE FROM pairs WHERE id = $1`

	result, err := r.db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("не удалось удалить пару: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("не удалось получить количество удалённых строк: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("пара с ID %d не найдена", id)
	}

	return nil
}

// === Операции с сделками (trades) ===

// SaveTrade сохраняет информацию о завершённой сделке.
// Возвращает ID созданной записи.
func (r *Repository) SaveTrade(trade *Trade) (int, error) {
	query := `
		INSERT INTO trades (pair_id, entry_time, exit_time, entry_spread, exit_spread,
		                    realized_pnl, exchange_long, exchange_short, volume, closed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`

	var id int
	err := r.db.QueryRow(
		query,
		trade.PairID,
		trade.EntryTime,
		trade.ExitTime,
		trade.EntrySpread,
		trade.ExitSpread,
		trade.RealizedPNL,
		trade.ExchangeLong,
		trade.ExchangeShort,
		trade.Volume,
		trade.ClosedBy,
	).Scan(&id)

	if err != nil {
		return 0, fmt.Errorf("не удалось сохранить сделку: %w", err)
	}

	return id, nil
}

// GetTradesByPairID получает все сделки для конкретной пары.
func (r *Repository) GetTradesByPairID(pairID int) ([]*Trade, error) {
	query := `
		SELECT id, pair_id, entry_time, exit_time, entry_spread, exit_spread,
		       realized_pnl, exchange_long, exchange_short, volume, closed_by, created_at
		FROM trades
		WHERE pair_id = $1
		ORDER BY entry_time DESC
	`

	rows, err := r.db.Query(query, pairID)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить сделки: %w", err)
	}
	defer rows.Close()

	var trades []*Trade
	for rows.Next() {
		var trade Trade
		err := rows.Scan(
			&trade.ID,
			&trade.PairID,
			&trade.EntryTime,
			&trade.ExitTime,
			&trade.EntrySpread,
			&trade.ExitSpread,
			&trade.RealizedPNL,
			&trade.ExchangeLong,
			&trade.ExchangeShort,
			&trade.Volume,
			&trade.ClosedBy,
			&trade.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("ошибка при сканировании сделки: %w", err)
		}
		trades = append(trades, &trade)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка при итерации по сделкам: %w", err)
	}

	return trades, nil
}

// === Операции с кэшем плеча (leverage_cache) ===

// GetLeverage получает закэшированное значение плеча.
// Возвращает (leverage, exists, error).
func (r *Repository) GetLeverage(exchange, symbol string) (int, bool, error) {
	query := `
		SELECT leverage
		FROM leverage_cache
		WHERE exchange = $1 AND symbol = $2
	`

	var leverage int
	err := r.db.QueryRow(query, exchange, symbol).Scan(&leverage)

	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ошибка при получении плеча из кэша: %w", err)
	}

	return leverage, true, nil
}

// SetLeverage сохраняет или обновляет значение плеча в кэше.
func (r *Repository) SetLeverage(exchange, symbol string, leverage int) error {
	query := `
		INSERT INTO leverage_cache (exchange, symbol, leverage, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (exchange, symbol)
		DO UPDATE SET leverage = EXCLUDED.leverage, updated_at = NOW()
	`

	_, err := r.db.Exec(query, exchange, symbol, leverage)
	if err != nil {
		return fmt.Errorf("не удалось сохранить плечо в кэш: %w", err)
	}

	return nil
}
