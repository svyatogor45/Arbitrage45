-- Создание таблицы leverage_cache для кэширования установленного плеча

CREATE TABLE leverage_cache (
    exchange VARCHAR(20) NOT NULL,
    symbol VARCHAR(20) NOT NULL,
    leverage INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (exchange, symbol)
);

-- Индекс для очистки старых записей
CREATE INDEX idx_leverage_updated_at ON leverage_cache(updated_at);

-- Комментарии к таблице и колонкам
COMMENT ON TABLE leverage_cache IS 'Кэш установленного плеча на биржах для избежания повторных API запросов';
COMMENT ON COLUMN leverage_cache.exchange IS 'Название биржи (bybit, bitget, bingx, gate, okx, htx, mexc)';
COMMENT ON COLUMN leverage_cache.symbol IS 'Символ торговой пары';
COMMENT ON COLUMN leverage_cache.leverage IS 'Установленное плечо (1-125x)';
COMMENT ON COLUMN leverage_cache.updated_at IS 'Время последнего обновления';
