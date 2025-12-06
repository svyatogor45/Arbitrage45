
# database.py
# ---------------------------------------------------
# Инициализация структуры SQLite базы данных.
# Поддерживает создание и миграцию:
#   • exchanges
#   • trading_pairs
#   • orders
#   • trade_events
#   • positions
#   • emergency_positions (NEW)
# ---------------------------------------------------

import sqlite3
from typing import List, Tuple

from config import DB_NAME, logger


class Database:
    def __init__(self, db_name: str = DB_NAME):
        self.db_name = db_name

    def get_connection(self) -> sqlite3.Connection:
        """Создаёт и возвращает соединение с базой данных."""
        conn = sqlite3.connect(self.db_name, timeout=10.0)
        conn.execute("PRAGMA journal_mode=WAL;")
        conn.execute("PRAGMA synchronous=NORMAL;")
        conn.execute("PRAGMA cache_size=-64000;")  # 64MB cache
        conn.execute("PRAGMA temp_store=MEMORY;")
        conn.execute("PRAGMA busy_timeout=5000;")
        return conn

    # ============================================================
    # ВСПОМОГАТЕЛЬНЫЕ МЕТОДЫ
    # ============================================================

    def _get_table_columns(self, cursor: sqlite3.Cursor, table_name: str) -> List[str]:
        """Вернуть список колонок таблицы."""
        cursor.execute(f"PRAGMA table_info({table_name})")
        rows: List[Tuple] = cursor.fetchall()
        return [row[1] for row in rows]

    def _ensure_column(self, cursor, table_name: str, column_name: str, column_def: str):
        """Если колонки нет — добавить через ALTER TABLE."""
        cols = self._get_table_columns(cursor, table_name)
        if column_name not in cols:
            logger.info(f"📦 DB: добавляем колонку {table_name}.{column_name}")
            cursor.execute(f"ALTER TABLE {table_name} ADD COLUMN {column_name} {column_def}")

    def _table_exists(self, cursor: sqlite3.Cursor, table_name: str) -> bool:
        """Проверить, существует ли таблица."""
        cursor.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name=?",
            (table_name,)
        )
        return cursor.fetchone() is not None

    def _index_exists(self, cursor: sqlite3.Cursor, index_name: str) -> bool:
        """Проверить, существует ли индекс."""
        cursor.execute(
            "SELECT name FROM sqlite_master WHERE type='index' AND name=?",
            (index_name,)
        )
        return cursor.fetchone() is not None

    def _create_index_if_not_exists(
        self, 
        cursor: sqlite3.Cursor, 
        index_name: str, 
        table_name: str, 
        columns: str
    ):
        """Создать индекс если не существует."""
        if not self._index_exists(cursor, index_name):
            logger.info(f"📦 DB: создаём индекс {index_name}")
            cursor.execute(f"CREATE INDEX {index_name} ON {table_name} ({columns})")

    # ============================================================
    # СОЗДАНИЕ И МИГРАЦИЯ БАЗЫ
    # ============================================================

    def init_db(self):
        """Создаёт структуру всех таблиц и добавляет недостающие поля."""
        conn = self.get_connection()
        cursor = conn.cursor()

        try:
            # ---------------------------------
            # 1. Биржевые аккаунты
            # ---------------------------------
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS exchanges (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    name TEXT NOT NULL UNIQUE,
                    api_key TEXT,
                    secret_key TEXT,
                    passphrase TEXT,
                    is_connected BOOLEAN DEFAULT 0,
                    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
                    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
                )
                """
            )
            
            # Миграция для exchanges
            self._ensure_column(cursor, "exchanges", "created_at", "DATETIME DEFAULT CURRENT_TIMESTAMP")
            self._ensure_column(cursor, "exchanges", "updated_at", "DATETIME DEFAULT CURRENT_TIMESTAMP")

            # ---------------------------------
            # 2. Торговые пары
            # ---------------------------------
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS trading_pairs (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    symbol TEXT NOT NULL,
                    exchange_a TEXT NOT NULL,
                    exchange_b TEXT NOT NULL,
                    volume REAL NOT NULL,
                    n_orders INTEGER DEFAULT 1,
                    entry_spread REAL NOT NULL,
                    exit_spread REAL NOT NULL,
                    stop_loss REAL,
                    status TEXT DEFAULT 'paused',
                    total_pnl REAL DEFAULT 0.0,
                    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
                )
                """
            )

            # Миграции для trading_pairs
            self._ensure_column(cursor, "trading_pairs", "sl_count", "INTEGER DEFAULT 0")
            self._ensure_column(cursor, "trading_pairs", "liq_count", "INTEGER DEFAULT 0")
            self._ensure_column(cursor, "trading_pairs", "last_sl_at", "DATETIME")
            self._ensure_column(cursor, "trading_pairs", "last_liq_at", "DATETIME")
            self._ensure_column(cursor, "trading_pairs", "created_at", "DATETIME DEFAULT CURRENT_TIMESTAMP")
            
            # Индексы для trading_pairs
            self._create_index_if_not_exists(cursor, "idx_trading_pairs_status", "trading_pairs", "status")
            self._create_index_if_not_exists(cursor, "idx_trading_pairs_symbol", "trading_pairs", "symbol")

            # ---------------------------------
            # 3. Таблица ордеров
            # ---------------------------------
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS orders (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    pair_id INTEGER,
                    exchange TEXT,
                    side TEXT,
                    price REAL,
                    amount REAL,
                    status TEXT,
                    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
                )
                """
            )

            # ---- Миграция timestamp → created_at ----
            columns = self._get_table_columns(cursor, "orders")

            if "timestamp" in columns and "created_at" not in columns:
                logger.info("📦 DB: миграция orders.timestamp → orders.created_at")
                cursor.execute("ALTER TABLE orders ADD COLUMN created_at DATETIME")
                cursor.execute("UPDATE orders SET created_at = timestamp")
                columns = self._get_table_columns(cursor, "orders")

            if "created_at" not in columns:
                self._ensure_column(cursor, "orders", "created_at", "DATETIME DEFAULT CURRENT_TIMESTAMP")

            # NEW: Дополнительные поля для orders
            self._ensure_column(cursor, "orders", "order_id", "TEXT")  # ID ордера с биржи
            self._ensure_column(cursor, "orders", "filled", "REAL")    # Реально исполненный объём
            self._ensure_column(cursor, "orders", "average_price", "REAL")  # Средняя цена исполнения
            
            # Индексы для orders
            self._create_index_if_not_exists(cursor, "idx_orders_pair_id", "orders", "pair_id")
            self._create_index_if_not_exists(cursor, "idx_orders_created_at", "orders", "created_at")
            self._create_index_if_not_exists(cursor, "idx_orders_exchange", "orders", "exchange")

            # ---------------------------------
            # 4. Журнал событий
            # ---------------------------------
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS trade_events (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    pair_id INTEGER,
                    event_type TEXT NOT NULL,
                    level TEXT DEFAULT 'info',
                    message TEXT,
                    meta TEXT,
                    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
                )
                """
            )
            
            # Индексы для trade_events
            self._create_index_if_not_exists(cursor, "idx_trade_events_pair_id", "trade_events", "pair_id")
            self._create_index_if_not_exists(cursor, "idx_trade_events_event_type", "trade_events", "event_type")
            self._create_index_if_not_exists(cursor, "idx_trade_events_level", "trade_events", "level")
            self._create_index_if_not_exists(cursor, "idx_trade_events_created_at", "trade_events", "created_at")

            # ---------------------------------
            # 5. Таблица активной позиции
            # ---------------------------------
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS positions (
                    pair_id INTEGER PRIMARY KEY,
                    long_exchange TEXT,
                    short_exchange TEXT,
                    filled_parts INTEGER DEFAULT 0,
                    closed_parts INTEGER DEFAULT 0,
                    entry_prices_long TEXT,
                    entry_prices_short TEXT,
                    part_volume REAL DEFAULT 0.0,
                    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
                )
                """
            )

            # Базовые колонки positions
            need_cols = {
                "long_exchange": "TEXT",
                "short_exchange": "TEXT",
                "filled_parts": "INTEGER DEFAULT 0",
                "closed_parts": "INTEGER DEFAULT 0",
                "entry_prices_long": "TEXT",
                "entry_prices_short": "TEXT",
                "part_volume": "REAL DEFAULT 0.0",
                "updated_at": "DATETIME DEFAULT CURRENT_TIMESTAMP",
            }

            for col, definition in need_cols.items():
                self._ensure_column(cursor, "positions", col, definition)

            # NEW: Дополнительные поля для positions (actual volumes)
            self._ensure_column(cursor, "positions", "actual_long_volume", "REAL")
            self._ensure_column(cursor, "positions", "actual_short_volume", "REAL")
            self._ensure_column(cursor, "positions", "exit_prices_long", "TEXT")
            self._ensure_column(cursor, "positions", "exit_prices_short", "TEXT")
            self._ensure_column(cursor, "positions", "created_at", "DATETIME DEFAULT CURRENT_TIMESTAMP")

            # ---------------------------------
            # 6. NEW: Emergency positions (зависшие позиции)
            # ---------------------------------
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS emergency_positions (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    pair_id INTEGER,
                    exchange TEXT NOT NULL,
                    symbol TEXT NOT NULL,
                    side TEXT NOT NULL,
                    amount REAL NOT NULL,
                    reason TEXT NOT NULL,
                    meta TEXT,
                    status TEXT DEFAULT 'pending',
                    resolution TEXT,
                    resolved_by TEXT,
                    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
                    resolved_at DATETIME
                )
                """
            )
            
            # Индексы для emergency_positions
            self._create_index_if_not_exists(
                cursor, "idx_emergency_positions_status", "emergency_positions", "status"
            )
            self._create_index_if_not_exists(
                cursor, "idx_emergency_positions_pair_id", "emergency_positions", "pair_id"
            )
            self._create_index_if_not_exists(
                cursor, "idx_emergency_positions_created_at", "emergency_positions", "created_at"
            )

            # ---------------------------------
            # 7. NEW: System metrics (метрики системы)
            # ---------------------------------
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS system_metrics (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    metric_name TEXT NOT NULL,
                    metric_value REAL,
                    metric_data TEXT,
                    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
                )
                """
            )
            
            self._create_index_if_not_exists(
                cursor, "idx_system_metrics_name", "system_metrics", "metric_name"
            )
            self._create_index_if_not_exists(
                cursor, "idx_system_metrics_created_at", "system_metrics", "created_at"
            )

            # ---------------------------------
            # 8. NEW: API credentials (для ExchangeManager)
            # ---------------------------------
            if not self._table_exists(cursor, "api_credentials"):
                cursor.execute(
                    """
                    CREATE TABLE api_credentials (
                        id INTEGER PRIMARY KEY AUTOINCREMENT,
                        exchange TEXT NOT NULL UNIQUE,
                        api_key TEXT,
                        secret_key TEXT,
                        passphrase TEXT,
                        is_active BOOLEAN DEFAULT 1,
                        created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
                        updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
                    )
                    """
                )
                logger.info("📦 DB: создана таблица api_credentials")

            # ---------------------------------
            # Завершение
            # ---------------------------------
            conn.commit()
            logger.info(f"📦 База данных '{self.db_name}' инициализирована/обновлена успешно.")

        except Exception as e:
            logger.error(f"❌ Ошибка инициализации базы: {e}")
            conn.rollback()
            raise
        finally:
            conn.close()

    # ============================================================
    # УТИЛИТЫ
    # ============================================================

    def get_table_stats(self) -> dict:
        """Получить статистику по таблицам."""
        conn = self.get_connection()
        cursor = conn.cursor()
        
        stats = {}
        tables = [
            "exchanges", "trading_pairs", "orders", 
            "trade_events", "positions", "emergency_positions",
            "system_metrics", "api_credentials"
        ]
        
        try:
            for table in tables:
                if self._table_exists(cursor, table):
                    cursor.execute(f"SELECT COUNT(*) FROM {table}")
                    count = cursor.fetchone()[0]
                    stats[table] = count
                else:
                    stats[table] = None
        finally:
            conn.close()
        
        return stats

    def vacuum(self):
        """Выполнить VACUUM для оптимизации БД."""
        conn = self.get_connection()
        try:
            conn.execute("VACUUM")
            logger.info("📦 DB: VACUUM выполнен")
        except Exception as e:
            logger.error(f"❌ Ошибка VACUUM: {e}")
        finally:
            conn.close()

    def cleanup_old_data(self, days: int = 30):
        """Удалить старые данные из логов и событий."""
        conn = self.get_connection()
        cursor = conn.cursor()
        
        try:
            # Удаляем старые события
            cursor.execute(
                """
                DELETE FROM trade_events 
                WHERE created_at < datetime('now', ?)
                """,
                (f'-{days} days',)
            )
            events_deleted = cursor.rowcount
            
            # Удаляем старые метрики
            cursor.execute(
                """
                DELETE FROM system_metrics 
                WHERE created_at < datetime('now', ?)
                """,
                (f'-{days} days',)
            )
            metrics_deleted = cursor.rowcount
            
            # Удаляем resolved emergency positions старше 90 дней
            cursor.execute(
                """
                DELETE FROM emergency_positions 
                WHERE status = 'resolved' 
                AND resolved_at < datetime('now', '-90 days')
                """
            )
            emergency_deleted = cursor.rowcount
            
            conn.commit()
            
            logger.info(
                f"📦 DB cleanup: удалено events={events_deleted}, "
                f"metrics={metrics_deleted}, emergency={emergency_deleted}"
            )
            
        except Exception as e:
            logger.error(f"❌ Ошибка cleanup: {e}")
            conn.rollback()
        finally:
            conn.close()


# ============================================================
# ENTRY POINT
# ============================================================

if __name__ == "__main__":
    db = Database()
    db.init_db()
    
    # Показать статистику
    stats = db.get_table_stats()
    print("\n📊 Статистика таблиц:")
    for table, count in stats.items():
        if count is not None:
            print(f"  {table}: {count} записей")
        else:
            print(f"  {table}: таблица не существует")
