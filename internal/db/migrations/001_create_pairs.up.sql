-- Создание таблицы pairs для хранения конфигураций торговых пар

CREATE TABLE pairs (
    id SERIAL PRIMARY KEY,
    symbol VARCHAR(20) NOT NULL,
    volume DECIMAL(20, 8) NOT NULL,
    entry_spread DECIMAL(10, 4) NOT NULL,
    exit_spread DECIMAL(10, 4) NOT NULL,
    num_orders INTEGER NOT NULL DEFAULT 1,
    stop_loss DECIMAL(20, 8),
    leverage INTEGER NOT NULL DEFAULT 1,
    status VARCHAR(20) NOT NULL DEFAULT 'PAUSED',
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Индексы для оптимизации запросов
CREATE INDEX idx_pairs_symbol ON pairs(symbol);
CREATE INDEX idx_pairs_status ON pairs(status);

-- Комментарии к таблице и колонкам
COMMENT ON TABLE pairs IS 'Конфигурация торговых пар для арбитража';
COMMENT ON COLUMN pairs.symbol IS 'Символ торговой пары (например, BTCUSDT)';
COMMENT ON COLUMN pairs.volume IS 'Объём для торговли в монетах актива';
COMMENT ON COLUMN pairs.entry_spread IS 'Порог входа в позицию (%)';
COMMENT ON COLUMN pairs.exit_spread IS 'Порог выхода из позиции (%)';
COMMENT ON COLUMN pairs.num_orders IS 'Количество частей для разбиения входа/выхода';
COMMENT ON COLUMN pairs.stop_loss IS 'Stop Loss в USDT (опционально)';
COMMENT ON COLUMN pairs.leverage IS 'Плечо (1-125x)';
COMMENT ON COLUMN pairs.status IS 'Текущий статус: PAUSED, READY, ENTERING, POSITION_OPEN, EXITING, ERROR';
