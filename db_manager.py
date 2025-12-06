# db_manager.py
# ---------------------------------------------------
# Надёжный менеджер SQLite для торгового ядра.
#
# Улучшения:
#   - Connection pool (persistent connection)
#   - Retry на SQLITE_BUSY
#   - Таблица emergency_positions
#   - Thread-safe операции
# ---------------------------------------------------

import sqlite3
import json
import time
import threading
from typing import List, Dict, Optional, Any, Callable
from contextlib import contextmanager
from dataclasses import dataclass

from config import DB_NAME, logger


# ============================================================
# КОНФИГУРАЦИЯ
# ============================================================

# Максимальное число retry при SQLITE_BUSY
MAX_BUSY_RETRIES = 5

# Базовая задержка между retry (секунды)
BUSY_RETRY_DELAY = 0.1

# Timeout для SQLite connection (секунды)
CONNECTION_TIMEOUT = 10.0

# Максимальный возраст connection перед переподключением (секунды)
CONNECTION_MAX_AGE = 300  # 5 минут


# ============================================================
# МЕТРИКИ
# ============================================================

@dataclass
class DBMetrics:
    """Метрики работы с БД."""
    queries_total: int = 0
    queries_success: int = 0
    queries_failed: int = 0
    busy_retries: int = 0
    connection_resets: int = 0
    
    def record_query(self, success: bool, retries: int = 0):
        self.queries_total += 1
        if success:
            self.queries_success += 1
        else:
            self.queries_failed += 1
        self.busy_retries += retries
    
    def to_dict(self) -> dict:
        return {
            "queries_total": self.queries_total,
            "queries_success": self.queries_success,
            "queries_failed": self.queries_failed,
            "success_rate": f"{(self.queries_success / self.queries_total * 100):.1f}%" if self.queries_total > 0 else "N/A",
            "busy_retries": self.busy_retries,
            "connection_resets": self.connection_resets,
        }


class DBManager:
    """
    Менеджер SQLite с connection pooling и retry логикой.
    
    Особенности:
    - Persistent connection с автоматическим переподключением
    - Retry при SQLITE_BUSY с exponential backoff
    - Thread-safe через threading.Lock
    - Метрики для мониторинга
    """
    
    def __init__(self, db_name: str = DB_NAME):
        self.db_name = db_name
        self._conn: Optional[sqlite3.Connection] = None
        self._conn_created_at: float = 0.0
        self._lock = threading.Lock()
        self.metrics = DBMetrics()

    # ============================================================
    # CONNECTION MANAGEMENT
    # ============================================================

    def _create_connection(self) -> sqlite3.Connection:
        """Создать новое подключение к SQLite."""
        conn = sqlite3.connect(
            self.db_name,
            timeout=CONNECTION_TIMEOUT,
            check_same_thread=False,  # Разрешаем использование из разных потоков
            isolation_level=None,  # Autocommit mode
        )
        conn.row_factory = sqlite3.Row
        
        # Оптимизации
        conn.execute("PRAGMA journal_mode = WAL;")
        conn.execute("PRAGMA synchronous = NORMAL;")
        conn.execute("PRAGMA cache_size = -64000;")  # 64MB cache
        conn.execute("PRAGMA temp_store = MEMORY;")
        conn.execute("PRAGMA busy_timeout = 5000;")  # 5 секунд
        
        return conn

    def _get_connection(self) -> sqlite3.Connection:
        """
        Получить connection (создать новый если нужно).
        Thread-safe.
        """
        with self._lock:
            now = time.time()
            
            # Проверяем, нужно ли переподключиться
            need_reconnect = (
                self._conn is None or
                (now - self._conn_created_at) > CONNECTION_MAX_AGE
            )
            
            if need_reconnect:
                if self._conn is not None:
                    try:
                        self._conn.close()
                    except Exception:
                        pass
                    self.metrics.connection_resets += 1
                
                self._conn = self._create_connection()
                self._conn_created_at = now
                
            return self._conn

    @contextmanager
    def _transaction(self):
        """
        Context manager для транзакции с retry на SQLITE_BUSY.
        """
        conn = self._get_connection()
        retries = 0
        
        while True:
            try:
                conn.execute("BEGIN IMMEDIATE;")
                yield conn
                conn.execute("COMMIT;")
                self.metrics.record_query(True, retries)
                return
                
            except sqlite3.OperationalError as e:
                error_msg = str(e).lower()
                
                # Retry на SQLITE_BUSY
                if "locked" in error_msg or "busy" in error_msg:
                    try:
                        conn.execute("ROLLBACK;")
                    except Exception:
                        pass
                    
                    retries += 1
                    if retries >= MAX_BUSY_RETRIES:
                        self.metrics.record_query(False, retries)
                        logger.error(
                            f"DB BUSY after {retries} retries: {e}"
                        )
                        raise
                    
                    # Exponential backoff
                    delay = BUSY_RETRY_DELAY * (2 ** (retries - 1))
                    logger.warning(
                        f"DB BUSY, retry {retries}/{MAX_BUSY_RETRIES} "
                        f"in {delay:.2f}s"
                    )
                    time.sleep(delay)
                    continue
                
                # Другие OperationalError
                try:
                    conn.execute("ROLLBACK;")
                except Exception:
                    pass
                self.metrics.record_query(False, retries)
                raise
                
            except Exception as e:
                try:
                    conn.execute("ROLLBACK;")
                except Exception:
                    pass
                self.metrics.record_query(False, retries)
                raise

    # ============================================================
    # ХЕЛПЕРЫ EXEC / FETCH
    # ============================================================

    def _execute(self, sql: str, params: tuple = ()) -> bool:
        """
        Выполнить SQL с retry логикой.
        Возвращает True при успехе.
        """
        try:
            with self._transaction() as conn:
                conn.execute(sql, params)
            return True
        except Exception as e:
            logger.error(f"DB EXEC ERROR: {e} | SQL={sql[:200]}")
            return False

    def _execute_many(self, sql: str, params_list: List[tuple]) -> bool:
        """Выполнить SQL для множества параметров."""
        try:
            with self._transaction() as conn:
                conn.executemany(sql, params_list)
            return True
        except Exception as e:
            logger.error(f"DB EXEC_MANY ERROR: {e} | SQL={sql[:200]}")
            return False

    def _fetchall(self, sql: str, params: tuple = ()) -> List[Dict[str, Any]]:
        """Получить все строки."""
        conn = self._get_connection()
        try:
            cur = conn.cursor()
            cur.execute(sql, params)
            rows = cur.fetchall()
            self.metrics.record_query(True)
            return [dict(r) for r in rows]
        except Exception as e:
            self.metrics.record_query(False)
            logger.error(f"DB FETCHALL ERROR: {e} | SQL={sql[:200]}")
            return []

    def _fetchone(self, sql: str, params: tuple = ()) -> Optional[Dict[str, Any]]:
        """Получить одну строку."""
        conn = self._get_connection()
        try:
            cur = conn.cursor()
            cur.execute(sql, params)
            row = cur.fetchone()
            self.metrics.record_query(True)
            return dict(row) if row else None
        except Exception as e:
            self.metrics.record_query(False)
            logger.error(f"DB FETCHONE ERROR: {e} | SQL={sql[:200]}")
            return None

    def _fetchval(self, sql: str, params: tuple = ()) -> Any:
        """Получить одно значение."""
        row = self._fetchone(sql, params)
        if row:
            return list(row.values())[0]
        return None

    # ============================================================
    # ТОРГОВЫЕ ПАРЫ
    # ============================================================

    def get_active_pairs(self) -> List[Dict[str, Any]]:
        """Получить все активные пары."""
        return self._fetchall(
            "SELECT * FROM trading_pairs WHERE status = 'active'"
        )

    def get_pair(self, pair_id: int) -> Optional[Dict[str, Any]]:
        """Получить пару по ID."""
        return self._fetchone(
            "SELECT * FROM trading_pairs WHERE id = ?",
            (pair_id,)
        )

    def get_pairs_by_status(self, status: str) -> List[Dict[str, Any]]:
        """Получить пары по статусу."""
        return self._fetchall(
            "SELECT * FROM trading_pairs WHERE status = ?",
            (status,)
        )

    def update_pair_status(self, pair_id: int, status: str) -> bool:
        """Обновить статус пары."""
        success = self._execute(
            "UPDATE trading_pairs SET status = ? WHERE id = ?",
            (status, pair_id)
        )
        if success:
            logger.info(f"📊 Пара {pair_id} статус → {status}")
        return success

    def update_pair_pnl(self, pair_id: int, pnl: float) -> bool:
        """Добавить PnL к паре."""
        success = self._execute(
            """
            UPDATE trading_pairs
            SET total_pnl = total_pnl + ?
            WHERE id = ?
            """,
            (pnl, pair_id)
        )
        if success:
            logger.info(f"💰 Пара {pair_id}: total_pnl += {pnl:.4f}")
        return success

    def increment_sl(self, pair_id: int) -> bool:
        """Инкрементировать счётчик SL."""
        success = self._execute(
            """
            UPDATE trading_pairs
            SET sl_count = sl_count + 1,
                last_sl_at = CURRENT_TIMESTAMP
            WHERE id = ?
            """,
            (pair_id,)
        )
        if success:
            logger.warning(f"⚠️ Пара {pair_id}: SL_COUNT++")
        return success

    def increment_liq(self, pair_id: int) -> bool:
        """Инкрементировать счётчик ликвидаций."""
        success = self._execute(
            """
            UPDATE trading_pairs
            SET liq_count = liq_count + 1,
                last_liq_at = CURRENT_TIMESTAMP
            WHERE id = ?
            """,
            (pair_id,)
        )
        if success:
            logger.error(f"🔥 Пара {pair_id}: LIQ_COUNT++")
        return success

    # ============================================================
    # ОРДЕРА (аудит)
    # ============================================================

    def save_order(
        self,
        pair_id: Optional[int],
        exchange: str,
        side: str,
        price: float,
        amount: float,
        status: str,
        order_id: Optional[str] = None,
        filled: Optional[float] = None,
    ) -> bool:
        """Сохранить ордер в аудит."""
        success = self._execute(
            """
            INSERT INTO orders (
                pair_id, exchange, side, price, amount, status, 
                order_id, filled, created_at
            )
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
            """,
            (pair_id, exchange, side, price, amount, status, order_id, filled)
        )

        if success:
            logger.bind(TRADE=True).debug(
                f"📝 ORDER pair={pair_id} ex={exchange} {side.upper()} "
                f"price={price} amount={amount} filled={filled} status={status}"
            )
        return success

    def get_orders_by_pair(
        self, 
        pair_id: int, 
        limit: int = 100
    ) -> List[Dict[str, Any]]:
        """Получить ордера по паре."""
        return self._fetchall(
            """
            SELECT * FROM orders 
            WHERE pair_id = ? 
            ORDER BY created_at DESC 
            LIMIT ?
            """,
            (pair_id, limit)
        )

    # ============================================================
    # СОБЫТИЯ ЛОГИРОВАНИЯ
    # ============================================================

    def log_trade_event(
        self,
        pair_id: Optional[int],
        event_type: str,
        level: str = "info",
        message: str = "",
        meta: Optional[dict] = None,
    ) -> bool:
        """Записать событие в лог."""
        meta_json = json.dumps(meta, ensure_ascii=False) if meta else None

        success = self._execute(
            """
            INSERT INTO trade_events (
                pair_id, event_type, level, message, meta, created_at
            )
            VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
            """,
            (pair_id, event_type, level, message, meta_json)
        )

        # Логируем в stdout
        if level == "warning":
            log_fn = logger.warning
        elif level == "error":
            log_fn = logger.error
        else:
            log_fn = logger.info

        log_fn(
            f"📌 EVENT [{event_type}] pair={pair_id} level={level} "
            f"msg='{message[:100]}'"
        )
        
        return success

    def get_events_by_pair(
        self,
        pair_id: int,
        limit: int = 100,
        level: Optional[str] = None,
    ) -> List[Dict[str, Any]]:
        """Получить события по паре."""
        if level:
            return self._fetchall(
                """
                SELECT * FROM trade_events 
                WHERE pair_id = ? AND level = ?
                ORDER BY created_at DESC 
                LIMIT ?
                """,
                (pair_id, level, limit)
            )
        return self._fetchall(
            """
            SELECT * FROM trade_events 
            WHERE pair_id = ? 
            ORDER BY created_at DESC 
            LIMIT ?
            """,
            (pair_id, limit)
        )

    # ============================================================
    # ПОЗИЦИИ
    # ============================================================

    def save_position(
        self,
        pair_id: int,
        long_exchange: str,
        short_exchange: str,
        filled_parts: int,
        closed_parts: int,
        entry_prices_long: List[float],
        entry_prices_short: List[float],
        part_volume: float,
        actual_long_volume: Optional[float] = None,
        actual_short_volume: Optional[float] = None,
    ) -> bool:
        """Сохранить или обновить активную позицию."""
        return self._execute(
            """
            INSERT INTO positions (
                pair_id, long_exchange, short_exchange,
                filled_parts, closed_parts,
                entry_prices_long, entry_prices_short,
                part_volume, actual_long_volume, actual_short_volume,
                updated_at
            )
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
            ON CONFLICT(pair_id) DO UPDATE SET
                long_exchange=excluded.long_exchange,
                short_exchange=excluded.short_exchange,
                filled_parts=excluded.filled_parts,
                closed_parts=excluded.closed_parts,
                entry_prices_long=excluded.entry_prices_long,
                entry_prices_short=excluded.entry_prices_short,
                part_volume=excluded.part_volume,
                actual_long_volume=excluded.actual_long_volume,
                actual_short_volume=excluded.actual_short_volume,
                updated_at=CURRENT_TIMESTAMP
            """,
            (
                pair_id,
                long_exchange,
                short_exchange,
                filled_parts,
                closed_parts,
                json.dumps(entry_prices_long),
                json.dumps(entry_prices_short),
                part_volume,
                actual_long_volume,
                actual_short_volume,
            )
        )

    def delete_position(self, pair_id: int) -> bool:
        """Удалить позицию после полного выхода."""
        return self._execute(
            "DELETE FROM positions WHERE pair_id = ?",
            (pair_id,)
        )

    def load_position(self, pair_id: int) -> Optional[Dict[str, Any]]:
        """Загрузить позицию по pair_id."""
        row = self._fetchone(
            "SELECT * FROM positions WHERE pair_id = ?",
            (pair_id,)
        )
        if not row:
            return None

        return self._parse_position_row(row)

    def load_all_positions(self) -> List[Dict[str, Any]]:
        """Загрузить все позиции."""
        rows = self._fetchall("SELECT * FROM positions")
        return [self._parse_position_row(r) for r in rows]

    def _parse_position_row(self, row: Dict[str, Any]) -> Dict[str, Any]:
        """Распарсить строку позиции."""
        try:
            row["entry_prices_long"] = json.loads(row.get("entry_prices_long") or "[]")
        except (json.JSONDecodeError, TypeError):
            row["entry_prices_long"] = []

        try:
            row["entry_prices_short"] = json.loads(row.get("entry_prices_short") or "[]")
        except (json.JSONDecodeError, TypeError):
            row["entry_prices_short"] = []

        return row

    # ============================================================
    # EMERGENCY POSITIONS (зависшие позиции)
    # ============================================================

    def save_emergency_position(
        self,
        pair_id: int,
        exchange: str,
        symbol: str,
        side: str,
        amount: float,
        reason: str,
        meta: Optional[dict] = None,
    ) -> bool:
        """
        Сохранить информацию о зависшей позиции для ручной обработки.
        """
        meta_json = json.dumps(meta, ensure_ascii=False) if meta else None
        
        success = self._execute(
            """
            INSERT INTO emergency_positions (
                pair_id, exchange, symbol, side, amount,
                reason, meta, status, created_at
            )
            VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', CURRENT_TIMESTAMP)
            """,
            (pair_id, exchange, symbol, side, amount, reason, meta_json)
        )
        
        if success:
            logger.critical(
                f"🚨 EMERGENCY POSITION SAVED | pair={pair_id} "
                f"{exchange} {side} {amount} {symbol} | reason={reason}"
            )
        return success

    def get_pending_emergency_positions(self) -> List[Dict[str, Any]]:
        """Получить все pending emergency позиции."""
        rows = self._fetchall(
            """
            SELECT * FROM emergency_positions 
            WHERE status = 'pending'
            ORDER BY created_at ASC
            """
        )
        for r in rows:
            try:
                r["meta"] = json.loads(r.get("meta") or "{}")
            except (json.JSONDecodeError, TypeError):
                r["meta"] = {}
        return rows

    def resolve_emergency_position(
        self,
        emergency_id: int,
        resolution: str,
        resolved_by: str = "system",
    ) -> bool:
        """Пометить emergency позицию как resolved."""
        return self._execute(
            """
            UPDATE emergency_positions
            SET status = 'resolved',
                resolution = ?,
                resolved_by = ?,
                resolved_at = CURRENT_TIMESTAMP
            WHERE id = ?
            """,
            (resolution, resolved_by, emergency_id)
        )

    # ============================================================
    # ВОССТАНОВЛЕНИЕ ПОСЛЕ РЕСТАРТА
    # ============================================================

    def get_open_positions_for_restore(self) -> List[Dict[str, Any]]:
        """Получить открытые позиции для восстановления после рестарта."""
        rows = self._fetchall(
            """
            SELECT
                p.pair_id,
                p.long_exchange,
                p.short_exchange,
                p.filled_parts,
                p.closed_parts,
                p.entry_prices_long,
                p.entry_prices_short,
                p.part_volume,
                p.actual_long_volume,
                p.actual_short_volume,
                tp.symbol,
                tp.volume      AS total_volume,
                tp.n_orders    AS n_orders,
                tp.entry_spread,
                tp.exit_spread,
                tp.stop_loss
            FROM positions p
            JOIN trading_pairs tp ON tp.id = p.pair_id
            """
        )

        result = []

        for r in rows:
            try:
                entry_prices_long = json.loads(r.get("entry_prices_long") or "[]")
            except (json.JSONDecodeError, TypeError):
                entry_prices_long = []
                
            try:
                entry_prices_short = json.loads(r.get("entry_prices_short") or "[]")
            except (json.JSONDecodeError, TypeError):
                entry_prices_short = []

            filled_parts = int(r.get("filled_parts") or 0)
            closed_parts = int(r.get("closed_parts") or 0)
            open_parts = max(0, filled_parts - closed_parts)

            if open_parts <= 0:
                continue

            n_orders = int(r.get("n_orders") or 1)

            # Определяем статус
            if filled_parts < n_orders and closed_parts == 0:
                status = "ENTERING"
            elif closed_parts > 0 and open_parts > 0:
                status = "EXITING"
            else:
                status = "HOLD"

            result.append({
                "pair_id": r["pair_id"],
                "symbol": r["symbol"],
                "total_volume": float(r.get("total_volume") or 0.0),
                "n_orders": n_orders,
                "entry_spread": float(r.get("entry_spread") or 0.0),
                "exit_spread": float(r.get("exit_spread") or 0.0),
                "stop_loss": float(r.get("stop_loss") or 0.0),
                "status": status,
                "long_exchange": r.get("long_exchange"),
                "short_exchange": r.get("short_exchange"),
                "filled_parts": filled_parts,
                "closed_parts": closed_parts,
                "entry_prices_long": entry_prices_long,
                "entry_prices_short": entry_prices_short,
                "exit_prices_long": [],
                "exit_prices_short": [],
                "part_volume": float(r.get("part_volume") or 0.0),
                "actual_long_volume": float(r.get("actual_long_volume") or 0.0),
                "actual_short_volume": float(r.get("actual_short_volume") or 0.0),
            })

        return result

    # ============================================================
    # ОЦЕНКА СУММАРНОГО РИСКА
    # ============================================================

    def get_total_open_notional(self) -> float:
        """Вычислить суммарный notional всех открытых позиций."""
        rows = self._fetchall("SELECT * FROM positions")

        total = 0.0

        for r in rows:
            try:
                entry_prices_long = json.loads(r.get("entry_prices_long") or "[]")
            except (json.JSONDecodeError, TypeError):
                entry_prices_long = []
                
            try:
                entry_prices_short = json.loads(r.get("entry_prices_short") or "[]")
            except (json.JSONDecodeError, TypeError):
                entry_prices_short = []

            filled_parts = int(r.get("filled_parts") or 0)
            closed_parts = int(r.get("closed_parts") or 0)
            open_parts = max(0, filled_parts - closed_parts)
            part_volume = float(r.get("part_volume") or 0.0)

            if open_parts <= 0 or part_volume <= 0:
                continue

            open_volume = open_parts * part_volume

            if not entry_prices_long and not entry_prices_short:
                continue

            avg_long = (
                sum(entry_prices_long) / len(entry_prices_long) 
                if entry_prices_long else 0.0
            )
            avg_short = (
                sum(entry_prices_short) / len(entry_prices_short) 
                if entry_prices_short else 0.0
            )

            if avg_long > 0 and avg_short > 0:
                avg_price = (avg_long + avg_short) / 2
            else:
                avg_price = max(avg_long, avg_short)

            if avg_price <= 0:
                continue

            total += open_volume * avg_price

        return total

    def get_open_positions_count(self) -> int:
        """Получить количество открытых позиций."""
        result = self._fetchval(
            """
            SELECT COUNT(*) FROM positions 
            WHERE filled_parts > closed_parts
            """
        )
        return int(result or 0)

    # ============================================================
    # СТАТИСТИКА
    # ============================================================

    def get_trading_stats(self) -> Dict[str, Any]:
        """Получить общую статистику торговли."""
        pairs_stats = self._fetchone(
            """
            SELECT 
                COUNT(*) as total_pairs,
                SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END) as active_pairs,
                SUM(CASE WHEN status = 'paused' THEN 1 ELSE 0 END) as paused_pairs,
                SUM(total_pnl) as total_pnl,
                SUM(sl_count) as total_sl,
                SUM(liq_count) as total_liq
            FROM trading_pairs
            """
        )
        
        positions_count = self.get_open_positions_count()
        total_notional = self.get_total_open_notional()
        
        return {
            "pairs": pairs_stats or {},
            "open_positions": positions_count,
            "total_notional": round(total_notional, 2),
            "db_metrics": self.metrics.to_dict(),
        }

    # ============================================================
    # CLEANUP / MAINTENANCE
    # ============================================================

    def cleanup_old_events(self, days: int = 30) -> int:
        """Удалить старые события."""
        conn = self._get_connection()
        try:
            cur = conn.cursor()
            cur.execute(
                """
                DELETE FROM trade_events 
                WHERE created_at < datetime('now', ?)
                """,
                (f'-{days} days',)
            )
            deleted = cur.rowcount
            logger.info(f"🧹 Удалено {deleted} старых событий (>{days} дней)")
            return deleted
        except Exception as e:
            logger.error(f"DB CLEANUP ERROR: {e}")
            return 0

    def vacuum(self):
        """Выполнить VACUUM для оптимизации БД."""
        conn = self._get_connection()
        try:
            conn.execute("VACUUM;")
            logger.info("🧹 VACUUM выполнен")
        except Exception as e:
            logger.error(f"DB VACUUM ERROR: {e}")

    # ============================================================
    # METRICS
    # ============================================================

    def get_metrics(self) -> dict:
        """Получить метрики работы с БД."""
        return self.metrics.to_dict()

    # ============================================================
    # CLOSE
    # ============================================================

    def close(self):
        """Закрыть соединение с БД."""
        with self._lock:
            if self._conn:
                try:
                    self._conn.close()
                except Exception:
                    pass
                self._conn = None
                logger.debug("🛑 DB connection closed")
