-- Откат миграции для таблицы leverage_cache

DROP INDEX IF EXISTS idx_leverage_updated_at;
DROP TABLE IF EXISTS leverage_cache;
