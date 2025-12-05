-- Откат миграции для таблицы pairs

DROP INDEX IF EXISTS idx_pairs_status;
DROP INDEX IF EXISTS idx_pairs_symbol;
DROP TABLE IF EXISTS pairs;
