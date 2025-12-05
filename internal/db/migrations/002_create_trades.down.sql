-- Откат миграции для таблицы trades

DROP INDEX IF EXISTS idx_trades_exit_time;
DROP INDEX IF EXISTS idx_trades_entry_time;
DROP INDEX IF EXISTS idx_trades_pair_id;
DROP TABLE IF EXISTS trades;
