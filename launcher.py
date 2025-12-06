# launcher.py
# ---------------------------------------------------
# Запуск торгового движка в отдельном процессе +
# восстановление позиций + autorestart + graceful shutdown.
#
# Улучшения:
#   - Обработка SIGTERM/SIGINT
#   - Health monitoring
#   - Метрики supervisor
#   - Exponential backoff при рестартах
# ---------------------------------------------------

import multiprocessing
import signal
import time
import sys
import os
import asyncio
from datetime import datetime
from typing import Optional, List
from dataclasses import dataclass, field

from loguru import logger

from db_manager import DBManager


# ============================================================
# КОНФИГУРАЦИЯ SUPERVISOR
# ============================================================

# Максимум рестартов в окне времени
RESTART_LIMIT = 5
RESTART_WINDOW = 60  # секунд

# Базовая задержка между рестартами
RESTART_DELAY_BASE = 5.0  # секунд
RESTART_DELAY_MAX = 60.0  # секунд
RESTART_BACKOFF_MULTIPLIER = 1.5

# Health check интервал
HEALTH_CHECK_INTERVAL = 30  # секунд

# Таймаут на graceful shutdown
SHUTDOWN_TIMEOUT = 30  # секунд


# ============================================================
# МЕТРИКИ SUPERVISOR
# ============================================================

@dataclass
class SupervisorMetrics:
    """Метрики работы supervisor."""
    started_at: datetime = field(default_factory=datetime.now)
    total_starts: int = 0
    total_crashes: int = 0
    last_start_at: Optional[datetime] = None
    last_crash_at: Optional[datetime] = None
    last_crash_reason: str = ""
    uptime_seconds: float = 0.0
    restart_times: List[float] = field(default_factory=list)
    
    def record_start(self):
        self.total_starts += 1
        self.last_start_at = datetime.now()
    
    def record_crash(self, reason: str = ""):
        self.total_crashes += 1
        self.last_crash_at = datetime.now()
        self.last_crash_reason = reason
        self.restart_times.append(time.time())
        # Очищаем старые записи
        now = time.time()
        self.restart_times = [t for t in self.restart_times if now - t < RESTART_WINDOW]
    
    def update_uptime(self, process_start_time: float):
        if process_start_time > 0:
            self.uptime_seconds = time.time() - process_start_time
    
    @property
    def crashes_in_window(self) -> int:
        now = time.time()
        return len([t for t in self.restart_times if now - t < RESTART_WINDOW])
    
    def to_dict(self) -> dict:
        return {
            "started_at": self.started_at.isoformat(),
            "total_starts": self.total_starts,
            "total_crashes": self.total_crashes,
            "crashes_in_window": self.crashes_in_window,
            "last_start": self.last_start_at.isoformat() if self.last_start_at else None,
            "last_crash": self.last_crash_at.isoformat() if self.last_crash_at else None,
            "last_crash_reason": self.last_crash_reason,
            "uptime_seconds": round(self.uptime_seconds, 2),
        }


# ============================================================
# ГЛОБАЛЬНЫЕ ПЕРЕМЕННЫЕ ДЛЯ SIGNAL HANDLING
# ============================================================

_shutdown_requested = False
_current_process: Optional[multiprocessing.Process] = None
_metrics = SupervisorMetrics()


def signal_handler(signum, frame):
    """Обработчик сигналов SIGTERM/SIGINT."""
    global _shutdown_requested
    
    sig_name = signal.Signals(signum).name
    logger.warning(f"🛑 Получен сигнал {sig_name}, запрашиваем graceful shutdown...")
    _shutdown_requested = True
    
    # Отправляем сигнал дочернему процессу
    if _current_process and _current_process.is_alive():
        logger.info("📤 Отправляем SIGTERM дочернему процессу...")
        try:
            _current_process.terminate()
        except Exception as e:
            logger.warning(f"⚠️ Ошибка отправки сигнала: {e}")


# ============================================================
# ВОССТАНОВЛЕНИЕ ПОЗИЦИЙ
# ============================================================

def restore_positions():
    """
    Логирует активные позиции для восстановления.
    Само восстановление происходит в main.py.
    """
    try:
        db = DBManager()
        rows = db.load_all_positions()
        
        if not rows:
            logger.info("📄 В таблице positions нет активных позиций — чистый запуск.")
            return 0
        
        logger.warning(f"♻ Найдено {len(rows)} позиций для восстановления:")
        for pos in rows:
            filled = pos.get('filled_parts', 0)
            closed = pos.get('closed_parts', 0)
            open_parts = max(0, filled - closed)
            
            if open_parts > 0:
                logger.warning(
                    f"  ▶ pair_id={pos.get('pair_id')} | "
                    f"open_parts={open_parts} | "
                    f"long={pos.get('long_exchange')} | "
                    f"short={pos.get('short_exchange')}"
                )
        
        db.close()
        return len(rows)
        
    except Exception as e:
        logger.error(f"❌ Ошибка при проверке позиций: {e}")
        return -1


def check_emergency_positions():
    """Проверить наличие pending emergency позиций."""
    try:
        db = DBManager()
        emergencies = db.get_pending_emergency_positions()
        db.close()
        
        if emergencies:
            logger.critical(
                f"🚨 ВНИМАНИЕ: Найдено {len(emergencies)} PENDING EMERGENCY позиций!"
            )
            for em in emergencies:
                logger.critical(
                    f"  🚨 ID={em.get('id')} | pair={em.get('pair_id')} | "
                    f"{em.get('exchange')} {em.get('side')} {em.get('amount')} | "
                    f"reason={em.get('reason')}"
                )
            return len(emergencies)
        return 0
        
    except Exception as e:
        logger.error(f"❌ Ошибка при проверке emergency позиций: {e}")
        return -1


# ============================================================
# ЗАПУСК ТОРГОВОГО ЯДРА
# ============================================================

def start_trading_engine():
    """
    Запуск торгового цикла внутри отдельного процесса.
    """
    # Устанавливаем обработчики сигналов в дочернем процессе
    signal.signal(signal.SIGTERM, lambda s, f: None)  # Игнорируем, main.py обработает
    signal.signal(signal.SIGINT, lambda s, f: None)
    
    logger.info("▶ Старт торгового цикла (внутри процесса)...")
    logger.info(f"  PID: {os.getpid()}")

    exit_code = 0
    
    try:
        # Проверяем позиции перед запуском
        positions_count = restore_positions()
        emergency_count = check_emergency_positions()
        
        if emergency_count > 0:
            logger.warning(
                f"⚠️ Есть {emergency_count} нерешённых emergency позиций. "
                f"Рекомендуется проверить вручную."
            )
        
        # Импортируем main здесь, чтобы избежать циклических импортов
        from main import main as trading_main
        
        # Запускаем основной цикл
        asyncio.run(trading_main())
        
    except KeyboardInterrupt:
        logger.info("⛔ TradingEngine остановлен по Ctrl+C")
        exit_code = 0
        
    except SystemExit as e:
        exit_code = e.code if isinstance(e.code, int) else 1
        logger.info(f"⚙ TradingEngine завершён с кодом {exit_code}")
        
    except Exception as e:
        logger.exception(f"🔥 Unhandled exception в торговом движке: {e}")
        exit_code = 1
        
    finally:
        logger.info(f"⚙ TradingEngine завершён (exit_code={exit_code})")
        sys.exit(exit_code)


# ============================================================
# РАСЧЁТ ЗАДЕРЖКИ РЕСТАРТА
# ============================================================

def calculate_restart_delay(consecutive_crashes: int) -> float:
    """Расчёт задержки с exponential backoff."""
    delay = RESTART_DELAY_BASE * (RESTART_BACKOFF_MULTIPLIER ** consecutive_crashes)
    return min(delay, RESTART_DELAY_MAX)


# ============================================================
# ОСНОВНОЙ SUPERVISOR
# ============================================================

def run_supervisor():
    """Основной цикл supervisor."""
    global _shutdown_requested, _current_process, _metrics
    
    logger.info("🚀 ЗАПУСК АРБИТРАЖНОГО ТЕРМИНАЛА (SUPERVISOR)")
    logger.info(f"  PID: {os.getpid()}")
    logger.info(f"  Restart limit: {RESTART_LIMIT} crashes per {RESTART_WINDOW}s")
    
    # Устанавливаем обработчики сигналов
    signal.signal(signal.SIGTERM, signal_handler)
    signal.signal(signal.SIGINT, signal_handler)
    
    consecutive_crashes = 0
    process_start_time = 0.0
    
    while not _shutdown_requested:
        # Запускаем процесс
        _current_process = multiprocessing.Process(
            target=start_trading_engine,
            name="TradingBot"
        )
        _current_process.start()
        process_start_time = time.time()
        
        _metrics.record_start()
        logger.info(f"✅ Торговый движок запущен (PID: {_current_process.pid})")
        
        # Ждём завершения процесса
        while _current_process.is_alive():
            try:
                _current_process.join(timeout=HEALTH_CHECK_INTERVAL)
                _metrics.update_uptime(process_start_time)
                
                if _shutdown_requested:
                    logger.info("🛑 Shutdown requested, ожидаем завершения процесса...")
                    _current_process.join(timeout=SHUTDOWN_TIMEOUT)
                    
                    if _current_process.is_alive():
                        logger.warning("⚠️ Процесс не завершился, принудительное завершение...")
                        _current_process.kill()
                        _current_process.join(timeout=5)
                    break
                    
            except KeyboardInterrupt:
                _shutdown_requested = True
                logger.info("🛑 Ctrl+C, запрашиваем shutdown...")
                continue
        
        # Процесс завершился
        exit_code = _current_process.exitcode
        _current_process = None
        
        if _shutdown_requested:
            logger.info(f"👋 Graceful shutdown завершён (exit_code={exit_code})")
            break
        
        # Анализируем причину завершения
        if exit_code == 0:
            logger.info("✅ Торговый движок завершился нормально (exit_code=0)")
            consecutive_crashes = 0
            # Нормальное завершение — не рестартим автоматически
            break
        else:
            # Краш
            consecutive_crashes += 1
            reason = f"exit_code={exit_code}"
            _metrics.record_crash(reason)
            
            logger.error(
                f"❗ TradingEngine завершился с ошибкой! "
                f"(exit_code={exit_code}, crash #{consecutive_crashes})"
            )
            
            # Проверяем лимит рестартов
            if _metrics.crashes_in_window >= RESTART_LIMIT:
                logger.critical(
                    f"❌ СЛИШКОМ МНОГО КРАШЕЙ ({_metrics.crashes_in_window} за "
                    f"{RESTART_WINDOW}s)! Autorestart отключён для безопасности."
                )
                
                # Логируем метрики
                logger.critical(f"📊 Supervisor metrics: {_metrics.to_dict()}")
                break
            
            # Рассчитываем задержку
            delay = calculate_restart_delay(consecutive_crashes - 1)
            logger.warning(
                f"🔁 Автоматический перезапуск через {delay:.1f}s... "
                f"(crashes in window: {_metrics.crashes_in_window}/{RESTART_LIMIT})"
            )
            
            # Ждём с возможностью прерывания
            wait_start = time.time()
            while time.time() - wait_start < delay:
                if _shutdown_requested:
                    logger.info("🛑 Shutdown requested во время ожидания рестарта")
                    break
                time.sleep(0.5)
            
            if _shutdown_requested:
                break
    
    # Финальный отчёт
    logger.info("=" * 50)
    logger.info("📊 SUPERVISOR ЗАВЕРШЁН")
    logger.info(f"  Total starts: {_metrics.total_starts}")
    logger.info(f"  Total crashes: {_metrics.total_crashes}")
    logger.info(f"  Last uptime: {_metrics.uptime_seconds:.1f}s")
    logger.info("=" * 50)
    
    return 0 if _metrics.total_crashes == 0 else 1


# ============================================================
# ENTRY POINT
# ============================================================

def main():
    """Entry point для запуска через python launcher.py."""
    try:
        exit_code = run_supervisor()
        sys.exit(exit_code)
    except Exception as e:
        logger.exception(f"🔥 Критическая ошибка в supervisor: {e}")
        sys.exit(1)


if __name__ == "__main__":
    main()
