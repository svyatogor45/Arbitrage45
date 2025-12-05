-- Создание таблицы trades для хранения истории арбитражных сделок

CREATE TABLE trades (
    id SERIAL PRIMARY KEY,
    pair_id INTEGER NOT NULL REFERENCES pairs(id) ON DELETE CASCADE,
    entry_time TIMESTAMP NOT NULL,
    exit_time TIMESTAMP,
    entry_spread DECIMAL(10, 4) NOT NULL,
    exit_spread DECIMAL(10, 4),
    realized_pnl DECIMAL(20, 8),
    exchange_long VARCHAR(20) NOT NULL,
    exchange_short VARCHAR(20) NOT NULL,
    volume DECIMAL(20, 8) NOT NULL,
    closed_by VARCHAR(20),
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Индексы для оптимизации запросов
CREATE INDEX idx_trades_pair_id ON trades(pair_id);
CREATE INDEX idx_trades_entry_time ON trades(entry_time);
CREATE INDEX idx_trades_exit_time ON trades(exit_time);

-- Комментарии к таблице и колонкам
COMMENT ON TABLE trades IS 'История арбитражных сделок';
COMMENT ON COLUMN trades.pair_id IS 'Ссылка на конфигурацию пары';
COMMENT ON COLUMN trades.entry_time IS 'Время открытия позиции';
COMMENT ON COLUMN trades.exit_time IS 'Время закрытия позиции';
COMMENT ON COLUMN trades.entry_spread IS 'Спред при входе (%)';
COMMENT ON COLUMN trades.exit_spread IS 'Спред при выходе (%)';
COMMENT ON COLUMN trades.realized_pnl IS 'Реализованный PNL в USDT';
COMMENT ON COLUMN trades.exchange_long IS 'Биржа для long позиции';
COMMENT ON COLUMN trades.exchange_short IS 'Биржа для short позиции';
COMMENT ON COLUMN trades.volume IS 'Общий объём сделки';
COMMENT ON COLUMN trades.closed_by IS 'Причина закрытия: target, stop_loss, liquidation, manual';
