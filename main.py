# main.py
# ---------------------------------------------------
# Главный цикл торгового ядра.
# В READY мы ИЩЕМ ЛУЧШУЮ СВЯЗКУ БИРЖ и направление
# (где покупать, где продавать) через MarketEngine.
#
# ИСПРАВЛЕНИЯ v2:
#   - Все вызовы execute_exit() передают pair_id
#   - Все вызовы execute_entry() передают pair_id
#   - position_info содержит pair_id для emergency positions
# ---------------------------------------------------

import asyncio
import signal
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Any, Tuple

from loguru import logger

from config import (
    PRICE_UPDATE_INTERVAL,
    EXCHANGES,
    MAX_TOTAL_RISK_USDT,
    MAX_PAIR_VOLUME,
    MAX_OPEN_PAIRS,
    MAX_MONITORED_PAIRS,
)

from db_manager import DBManager
from ws_manager import WsManager
from exchange_manager import ExchangeManager
from market_engine import MarketEngine
from trade_engine import TradeEngine


# ==============================
# Константы статусов пары
# ==============================

STATE_READY = "READY"
STATE_ENTERING = "ENTERING"
STATE_HOLD = "HOLD"
STATE_EXITING = "EXITING"
STATE_PAUSED = "PAUSED"
STATE_ERROR = "ERROR"


# ==============================
# Класс состояния пары в памяти
# ==============================

@dataclass
class PairState:
    """
    Состояние конкретной пары в оперативной памяти.

    Здесь лежит только то, что нужно для быстрой работы логики:
      - общие параметры пары (объём, n_orders, пороги спреда, SL),
      - сколько частей уже вошли / вышли,
      - какие биржи используются под long/short,
      - накопленные цены входа/выхода для расчёта PnL.
    """

    pair_id: int
    symbol: str
    total_volume: float
    n_orders: int
    entry_spread: float
    exit_spread: float
    stop_loss: float

    # динамические поля
    part_volume: float = field(init=False)
    filled_parts: int = 0           # сколько частей ВХОДА уже открыто
    closed_parts: int = 0           # сколько частей уже ЗАКРЫТО (по обычному TP)
    long_exchange: Optional[str] = None
    short_exchange: Optional[str] = None
    entry_prices_long: List[float] = field(default_factory=list)
    entry_prices_short: List[float] = field(default_factory=list)
    exit_prices_long: List[float] = field(default_factory=list)
    exit_prices_short: List[float] = field(default_factory=list)
    status: str = STATE_READY
    
    # Реальные исполненные объёмы (для контроля дисбаланса)
    actual_long_volume: float = 0.0
    actual_short_volume: float = 0.0

    def __post_init__(self):
        # Гарантируем, что число частей >= 1
        self.n_orders = max(1, int(self.n_orders))
        self.part_volume = self.total_volume / self.n_orders if self.n_orders > 0 else 0.0

    @property
    def is_flat(self) -> bool:
        """
        Позиция полностью закрыта (нет ни одной открытой части).
        """
        return self.filled_parts == 0 and self.closed_parts == 0

    @property
    def is_fully_entered(self) -> bool:
        """
        Все части входа уже открыты.
        """
        return self.filled_parts >= self.n_orders

    @property
    def open_parts(self) -> int:
        """
        Сколько частей сейчас открыто (учитывая частичное закрытие).
        """
        return max(0, self.filled_parts - self.closed_parts)

    @property
    def open_volume(self) -> float:
        """
        Объём по ещё ОТКРЫТЫМ частям.
        """
        return self.open_parts * self.part_volume
    
    @property
    def volume_imbalance(self) -> float:
        """
        Дисбаланс между LONG и SHORT объёмами.
        Положительное значение = LONG больше, отрицательное = SHORT больше.
        """
        return self.actual_long_volume - self.actual_short_volume

    def reset_after_exit(self):
        """
        Полный сброс состояния после нормального выхода/SL.
        Пара возвращается в состояние READY.
        """
        self.filled_parts = 0
        self.closed_parts = 0
        self.long_exchange = None
        self.short_exchange = None
        self.entry_prices_long.clear()
        self.entry_prices_short.clear()
        self.exit_prices_long.clear()
        self.exit_prices_short.clear()
        self.actual_long_volume = 0.0
        self.actual_short_volume = 0.0
        self.status = STATE_READY


# ==============================
# Глобальный контроллер риска (thread-safe)
# ==============================

class RiskController:
    """
    Атомарный контроллер риск-лимитов.
    Защищает от race condition при параллельном входе нескольких пар.
    """
    
    def __init__(self, db: DBManager):
        self.db = db
        self._lock = asyncio.Lock()
        self._open_pairs_count: int = 0
        self._current_risk_usdt: float = 0.0
    
    async def refresh_from_state(self, pair_states: Dict[int, "PairState"]):
        """
        Пересчитать лимиты из текущего состояния пар.
        Вызывается в начале каждого тика.

        ИСПРАВЛЕНО: создаём snapshot для избежания race condition.
        """
        async with self._lock:
            # Создаём snapshot для безопасной итерации (защита от race condition)
            snapshot = dict(pair_states)
            self._open_pairs_count = sum(
                1 for s in snapshot.values()
                if s.open_parts > 0
            )
            try:
                self._current_risk_usdt = float(self.db.get_total_open_notional())
            except (AttributeError, TypeError):
                self._current_risk_usdt = 0.0
    
    async def try_acquire_entry_slot(
        self,
        planned_notional: float,
    ) -> Tuple[bool, str]:
        """
        Атомарная попытка занять слот для входа.
        
        Возвращает (success, reason).
        Если success=True, слот уже зарезервирован и риск учтён.
        """
        async with self._lock:
            # Проверка лимита по количеству пар
            remaining_slots = MAX_OPEN_PAIRS - self._open_pairs_count
            if remaining_slots <= 0:
                return False, "NO_ENTRY_SLOTS"
            
            # Проверка лимита по размеру одной позиции
            if MAX_PAIR_VOLUME is not None and MAX_PAIR_VOLUME > 0:
                if planned_notional > MAX_PAIR_VOLUME:
                    return False, "PAIR_VOLUME_EXCEEDS_MAX"
            
            # Проверка глобального лимита риска
            if MAX_TOTAL_RISK_USDT is not None and MAX_TOTAL_RISK_USDT > 0:
                if self._current_risk_usdt + planned_notional > MAX_TOTAL_RISK_USDT:
                    return False, "TOTAL_RISK_LIMIT_EXCEEDED"
            
            # Всё ок — резервируем слот АТОМАРНО
            self._open_pairs_count += 1
            self._current_risk_usdt += planned_notional
            
            return True, "OK"
    
    async def release_entry_slot(self, planned_notional: float):
        """
        Освободить слот, если вход не состоялся.
        """
        async with self._lock:
            self._open_pairs_count = max(0, self._open_pairs_count - 1)
            self._current_risk_usdt = max(0.0, self._current_risk_usdt - planned_notional)
    
    def get_snapshot(self) -> Dict[str, Any]:
        """
        Получить текущий снимок риск-лимитов (для логирования).
        Не thread-safe, использовать только для чтения/логов.
        """
        return {
            "open_pairs_count": self._open_pairs_count,
            "remaining_slots": max(0, MAX_OPEN_PAIRS - self._open_pairs_count),
            "current_risk_usdt": self._current_risk_usdt,
            "max_risk_usdt": MAX_TOTAL_RISK_USDT,
        }


# ==============================
# Вспомогательные функции риска
# ==============================

def estimate_planned_position_notional(state: PairState, signal: dict) -> float:
    """
    Оценка "стоимости" позиции в USDT для риск-лимитов:
    считаем сразу по ВСЕМ частям, а не только по первой.
    """
    total_volume = state.total_volume
    buy_price = float(signal.get("buy_price", 0.0))
    sell_price = float(signal.get("sell_price", 0.0))
    if buy_price and sell_price:
        avg_price = (buy_price + sell_price) / 2.0
    else:
        avg_price = max(buy_price, sell_price, 0.0)
    return total_volume * avg_price


# ==============================
# Graceful Shutdown Manager
# ==============================

class ShutdownManager:
    """
    Управляет graceful shutdown:
    - перехват SIGTERM/SIGINT
    - закрытие всех открытых позиций перед выходом
    """
    
    def __init__(self):
        self._shutdown_requested = False
        self._shutdown_event = asyncio.Event()
    
    @property
    def is_shutdown_requested(self) -> bool:
        return self._shutdown_requested
    
    def request_shutdown(self):
        """Запросить остановку."""
        if not self._shutdown_requested:
            self._shutdown_requested = True
            self._shutdown_event.set()
            logger.warning("🛑 Shutdown requested!")
    
    async def wait_for_shutdown(self):
        """Ожидать сигнала остановки."""
        await self._shutdown_event.wait()
    
    def setup_signal_handlers(self, loop: asyncio.AbstractEventLoop):
        """Установить обработчики сигналов."""
        for sig in (signal.SIGTERM, signal.SIGINT):
            try:
                loop.add_signal_handler(sig, self.request_shutdown)
                logger.debug(f"Signal handler installed for {sig.name}")
            except NotImplementedError:
                # Windows не поддерживает add_signal_handler
                logger.warning(f"Cannot install signal handler for {sig.name} on this platform")


async def graceful_close_all_positions(
    pair_states: Dict[int, PairState],
    trader: TradeEngine,
    db: DBManager,
):
    """
    Аварийное закрытие всех открытых позиций при shutdown.
    """
    open_positions = [
        state for state in pair_states.values()
        if state.open_parts > 0 and state.long_exchange and state.short_exchange
    ]
    
    if not open_positions:
        logger.info("📭 Нет открытых позиций для закрытия при shutdown")
        return
    
    logger.warning(f"🚨 GRACEFUL SHUTDOWN: закрываем {len(open_positions)} позиций...")
    
    tasks = []
    for state in open_positions:
        tasks.append(
            _emergency_close_position(state, trader, db)
        )
    
    results = await asyncio.gather(*tasks, return_exceptions=True)
    
    success_count = sum(1 for r in results if r is True)
    fail_count = len(results) - success_count
    
    logger.info(
        f"📊 Shutdown close results: {success_count} успешно, {fail_count} с ошибками"
    )


# ИСПРАВЛЕНО: упрощена сигнатура, position_info строится внутри
async def _emergency_close_position(
    state: PairState,
    trader: TradeEngine,
    db: DBManager,
) -> bool:
    """Закрыть одну позицию при emergency shutdown."""
    try:
        # ИСПРАВЛЕНО: добавлен pair_id в position_info
        position_info = {
            "symbol": state.symbol,
            "long_exchange": state.long_exchange,
            "short_exchange": state.short_exchange,
            "pair_id": state.pair_id,
        }
        
        res = await trader.execute_exit(position_info, state.open_volume)
        
        if res["success"]:
            logger.info(f"✅ [{state.pair_id}] Позиция закрыта при shutdown")
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="EMERGENCY_CLOSE_OK",
                level="warning",
                message="Позиция закрыта при graceful shutdown",
                meta={"symbol": state.symbol},
            )
            return True
        else:
            logger.error(
                f"❌ [{state.pair_id}] Не удалось закрыть позицию при shutdown: "
                f"{res.get('error')}"
            )
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="EMERGENCY_CLOSE_FAILED",
                level="error",
                message=f"Ошибка закрытия при shutdown: {res.get('error')}",
                meta={"symbol": state.symbol},
            )
            return False
    except Exception as e:
        logger.exception(f"❌ [{state.pair_id}] Exception при emergency close: {e}")
        return False


# ==============================
# Основной торговый цикл
# ==============================

async def main():
    logger.info("🚀 ТОРГОВОЕ ЯДРО ЗАПУЩЕНО (LIVE MODE)")

    # Инициализация shutdown manager
    shutdown_mgr = ShutdownManager()
    loop = asyncio.get_running_loop()
    shutdown_mgr.setup_signal_handlers(loop)

    db = DBManager()
    ws_manager = WsManager()

    # Всегда используем боевой ExchangeManager
    ex_manager = ExchangeManager()
    logger.info("💹 LIVE MODE: используется реальный ExchangeManager (CCXT).")

    market = MarketEngine(ws_manager)
    trader = TradeEngine(ex_manager, db)
    
    # Атомарный контроллер рисков
    risk_controller = RiskController(db)

    # Запуск WebSocket-потоков (в фоне)
    asyncio.create_task(ws_manager.start())
    logger.info("📡 WebSocket менеджер запущен")

    # ИСПРАВЛЕНИЕ баг #6: Периодическая очистка кэша спредов
    async def periodic_cache_cleanup():
        """Очистка устаревших записей из кэша спредов MarketEngine."""
        while not shutdown_mgr.is_shutdown_requested:
            await asyncio.sleep(60)  # каждую минуту
            try:
                market.cleanup_stale_cache()
            except Exception as e:
                logger.warning(f"⚠️ Ошибка очистки кэша спредов: {e}")

    asyncio.create_task(periodic_cache_cleanup())

    # Состояния пар в памяти: pair_id -> PairState
    pair_states: Dict[int, PairState] = {}

    # ------------------------------
    # Восстановление позиций из БД (3.1)
    # ------------------------------
    try:
        open_positions = db.get_open_positions_for_restore()
    except AttributeError:
        open_positions = []
        logger.info(
            "DBManager не поддерживает get_open_positions_for_restore(), "
            "восстановление позиций пропущено."
        )
    else:
        if open_positions:
            logger.info(f"🔄 Найдено открытых позиций для восстановления: {len(open_positions)}")
        for pos in open_positions:
            try:
                state = PairState(
                    pair_id=pos["pair_id"],
                    symbol=pos["symbol"],
                    total_volume=float(pos["total_volume"]),
                    n_orders=int(pos["n_orders"]),
                    entry_spread=float(pos["entry_spread"]),
                    exit_spread=float(pos["exit_spread"]),
                    stop_loss=float(pos.get("stop_loss") or 0.0),
                )
                # Накатываем динамическое состояние из БД
                state.status = pos.get("status", STATE_HOLD)
                state.long_exchange = pos.get("long_exchange")
                state.short_exchange = pos.get("short_exchange")
                state.filled_parts = int(pos.get("filled_parts", 0))
                state.closed_parts = int(pos.get("closed_parts", 0))
                state.entry_prices_long = list(pos.get("entry_prices_long", []))
                state.entry_prices_short = list(pos.get("entry_prices_short", []))
                state.exit_prices_long = list(pos.get("exit_prices_long", []))
                state.exit_prices_short = list(pos.get("exit_prices_short", []))

                pair_states[state.pair_id] = state

                logger.info(
                    f"🔁 Восстановлена позиция по паре {state.pair_id} ({state.symbol}): "
                    f"status={state.status}, filled={state.filled_parts}, "
                    f"closed={state.closed_parts}, long={state.long_exchange}, "
                    f"short={state.short_exchange}"
                )
            except Exception as e:
                logger.error(
                    f"Ошибка при восстановлении позиции из БД для pair_id={pos.get('pair_id')}: {e}"
                )

    try:
        while not shutdown_mgr.is_shutdown_requested:
            try:
                # Загружаем активные пары (status='active')
                pairs = db.get_active_pairs()

                # Если активных пар нет — просто ждём и идём в следующий тик
                if not pairs:
                    await asyncio.sleep(PRICE_UPDATE_INTERVAL)
                    continue

                # Ограничиваем максимумом в один тик (страховка от перегруза)
                if len(pairs) > MAX_MONITORED_PAIRS:
                    logger.warning(
                        f"Активных пар {len(pairs)}, но одновременно мониторим "
                        f"только {MAX_MONITORED_PAIRS}. Лишние пары будут "
                        f"пропущены в этом цикле."
                    )
                    pairs = pairs[:MAX_MONITORED_PAIRS]

                # id активных пар в этом цикле
                active_ids = {p["id"] for p in pairs}

                # Чистим локальные состояния тех пар, которые ушли из активных
                for pid in list(pair_states.keys()):
                    if pid not in active_ids:
                        logger.info(
                            f"🧹 Пара {pid} больше не активна, "
                            f"удаляем состояние из памяти"
                        )
                        pair_states.pop(pid, None)

                # ------------------------------
                # Обновляем риск-контроллер из текущего состояния
                # ------------------------------
                await risk_controller.refresh_from_state(pair_states)
                
                risk_snapshot = risk_controller.get_snapshot()
                if risk_snapshot["remaining_slots"] <= 0:
                    logger.debug(
                        f"🌐 Глобальный лимит по количеству пар: "
                        f"открыто {risk_snapshot['open_pairs_count']}, "
                        f"новые входы в этом цикле запрещены."
                    )

                # ------------------------------
                # Готовим задачи по ВСЕМ активным парам
                # ------------------------------
                tasks: List[asyncio.Future] = []

                for p in pairs:
                    pair_id = p["id"]
                    symbol = p["symbol"]
                    volume = float(p["volume"])
                    n_orders = int(p["n_orders"])
                    entry_spread = float(p["entry_spread"])
                    exit_spread = float(p["exit_spread"])
                    stop_loss = float(p["stop_loss"]) if p["stop_loss"] is not None else 0.0

                    state = pair_states.get(pair_id)
                    if state is None:
                        state = PairState(
                            pair_id=pair_id,
                            symbol=symbol,
                            total_volume=volume,
                            n_orders=n_orders,
                            entry_spread=entry_spread,
                            exit_spread=exit_spread,
                            stop_loss=stop_loss,
                        )
                        pair_states[pair_id] = state
                        logger.info(
                            f"➕ Создано состояние для пары {pair_id} ({symbol}), "
                            f"статус={state.status}"
                        )

                    # Гарантируем подписку на стаканы по этому символу для всех бирж
                    for ex in EXCHANGES:
                        await ws_manager.subscribe(ex, symbol)

                    # Задача на один тик по конкретной паре
                    tasks.append(
                        handle_pair_cycle(
                            db=db,
                            market=market,
                            trader=trader,
                            pair_row=p,
                            state=state,
                            risk_controller=risk_controller,
                        )
                    )

                # Одним await обрабатываем ВСЕ пары этого тика параллельно
                if tasks:
                    await asyncio.gather(*tasks)

                # Пауза между тиками логики (а не между парами)
                await asyncio.sleep(PRICE_UPDATE_INTERVAL)

            except Exception as e:
                logger.error(f"🔥 Ошибка в главном цикле: {e}")
                # Небольшая пауза, чтобы не крутить ошибку в tight loop
                await asyncio.sleep(2)

    except KeyboardInterrupt:
        logger.info("🛑 KeyboardInterrupt в main()")
        shutdown_mgr.request_shutdown()

    finally:
        logger.info("🛑 Начинаем graceful shutdown...")
        
        # Закрываем все открытые позиции
        await graceful_close_all_positions(pair_states, trader, db)
        
        # Останавливаем WebSocket и биржевые подключения
        logger.info("🛑 Остановка WebSocket и биржевых подключений...")
        await ws_manager.stop()
        await ex_manager.close_all()
        logger.info("👋 Торговое ядро остановлено.")


# ==============================
# Обработка одной пары за один тик
# ==============================

async def handle_pair_cycle(
    db: DBManager,
    market: MarketEngine,
    trader: TradeEngine,
    pair_row: dict,
    state: PairState,
    risk_controller: RiskController,
):
    """
    Один шаг обработки конкретной пары:
      - в зависимости от state.status выполняем вход / сопровождение / выход.
    """
    if state.status == STATE_READY:
        await handle_state_ready(db, market, trader, pair_row, state, risk_controller)
    elif state.status == STATE_ENTERING:
        await handle_state_entering(db, market, trader, pair_row, state)
    elif state.status == STATE_HOLD:
        await handle_state_hold(db, market, trader, pair_row, state)
    elif state.status == STATE_EXITING:
        await handle_state_exiting(db, market, trader, pair_row, state)
    elif state.status in (STATE_PAUSED, STATE_ERROR):
        # Ничего не делаем — пара будет убрана из цикла,
        # когда в БД сменится статус.
        return
    else:
        logger.warning(f"⚠ Неизвестный статус пары {state.pair_id}: {state.status}")


# ==============================
# READY → попытка входа первой частью
# ==============================

async def handle_state_ready(
    db: DBManager,
    market: MarketEngine,
    trader: TradeEngine,
    pair_row: dict,
    state: PairState,
    risk_controller: RiskController,
):
    """
    Состояние READY:
      - позиция ещё не открыта (flat);
      - через MarketEngine.find_best_opportunity() ищем ЛУЧШУЮ связку бирж
        и направление (где покупать, где продавать) для этого символа;
      - при выполнении условий и наличии глобальных слотов/рисковых лимитов —
        заходим первой частью.
    """
    if not state.is_flat:
        logger.warning(
            f"[{state.pair_id}] READY, но позиция не flat "
            f"(filled={state.filled_parts}, closed={state.closed_parts})"
        )
        return

    symbol = state.symbol
    monitor_volume = state.part_volume

    # Вместо фиксированных exchange_a/exchange_b из БД перебираем ВСЕ биржи,
    # указанные в EXCHANGES, и ищем лучшую возможность.
    try:
        signal = await market.find_best_opportunity(
            symbol=symbol,
            volume_in_coin=monitor_volume,
            exchanges=EXCHANGES,              # ограничиваемся списком из config
            min_spread_pct=state.entry_spread # сразу фильтруем по порогу входа
        )
    except AttributeError:
        logger.error(
            f"[{state.pair_id}] MarketEngine не поддерживает find_best_opportunity(). "
            f"Нужна актуальная версия market_engine.py."
        )
        state.status = STATE_ERROR
        return

    if not signal:
        # Нет ни одной связки бирж/направления, удовлетворяющих entry_spread
        logger.debug(
            f"[{state.pair_id}] READY | нет подходящих возможностей по {symbol} "
            f"при entry_spread >= {state.entry_spread}%"
        )
        return

    # Оценка стоимости позиции для риск-лимитов
    planned_notional = estimate_planned_position_notional(state, signal)
    
    # АТОМАРНАЯ проверка и резервирование слота
    allowed, reason = await risk_controller.try_acquire_entry_slot(planned_notional)
    
    if not allowed:
        logger.debug(
            f"[{state.pair_id}] READY → вход ОТКЛОНЁН ({symbol}), причина={reason}"
        )
        return

    # Слот зарезервирован — пробуем войти
    net_spread = signal["net_full_spread_pct"]
    buy_ex = signal["buy_exchange"]
    sell_ex = signal["sell_exchange"]

    logger.info(
        f"[{state.pair_id}] READY → ENTRY SIGNAL {symbol} | "
        f"{buy_ex}->{sell_ex} spread={net_spread}% (>= {state.entry_spread}%)"
    )

    # ИСПРАВЛЕНО: передаём pair_id
    res = await trader.execute_entry(signal, monitor_volume, pair_id=state.pair_id)
    
    if res["success"]:
        # Проверяем реально исполненные объёмы
        long_order = res.get("entry_long_order", {})
        short_order = res.get("entry_short_order", {})
        
        filled_long = float(long_order.get("filled") or monitor_volume)
        filled_short = float(short_order.get("filled") or monitor_volume)
        
        # Сохраняем биржи и цены входа по первой части
        state.long_exchange = buy_ex
        state.short_exchange = sell_ex
        state.filled_parts = 1
        state.closed_parts = 0
        state.entry_prices_long = [signal["buy_price"]]
        state.entry_prices_short = [signal["sell_price"]]
        state.actual_long_volume = filled_long
        state.actual_short_volume = filled_short
        
        # Проверяем дисбаланс
        imbalance = abs(filled_long - filled_short)
        imbalance_pct = (imbalance / monitor_volume * 100) if monitor_volume > 0 else 0
        
        if imbalance_pct > 5:  # Дисбаланс более 5%
            logger.warning(
                f"[{state.pair_id}] ⚠️ VOLUME IMBALANCE: "
                f"LONG={filled_long:.6f}, SHORT={filled_short:.6f}, "
                f"diff={imbalance_pct:.2f}%"
            )
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="VOLUME_IMBALANCE",
                level="warning",
                message=f"Дисбаланс объёмов при входе: {imbalance_pct:.2f}%",
                meta={
                    "filled_long": filled_long,
                    "filled_short": filled_short,
                    "requested": monitor_volume,
                },
            )

        # Сохраняем позицию в БД (БЛОК 4)
        db.save_position(
            pair_id=state.pair_id,
            long_exchange=state.long_exchange,
            short_exchange=state.short_exchange,
            filled_parts=state.filled_parts,
            closed_parts=state.closed_parts,
            entry_prices_long=state.entry_prices_long,
            entry_prices_short=state.entry_prices_short,
            part_volume=state.part_volume,
        )

        db.log_trade_event(
            pair_id=state.pair_id,
            event_type="ENTRY_OK",
            level="info",
            message=(
                f"Первый вход по {symbol}: "
                f"{state.long_exchange}->{state.short_exchange}, часть 1/{state.n_orders}"
            ),
            meta={
                "buy_exchange": state.long_exchange,
                "sell_exchange": state.short_exchange,
                "volume": monitor_volume,
                "filled_long": filled_long,
                "filled_short": filled_short,
                "spread_pct": net_spread,
                "dynamic_selection": True,
            },
        )

        # Если нужно ещё добирать — ENTERING, иначе сразу HOLD
        state.status = STATE_ENTERING if state.n_orders > 1 else STATE_HOLD

    else:
        # Вход не удался — ОСВОБОЖДАЕМ зарезервированный слот
        await risk_controller.release_entry_slot(planned_notional)
        
        error_code = res.get("error") or "ENTRY_ERROR"
        if error_code == "second_leg_failed_emergency_close":
            db.update_pair_status(state.pair_id, "paused")
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="SECOND_LEG_FAILED",
                level="error",
                message=(
                    "Ошибка второй ноги при входе, LONG закрыт аварийно. "
                    "Пара поставлена на паузу."
                ),
                meta={
                    "symbol": symbol,
                    "buy_exchange": buy_ex,
                    "sell_exchange": sell_ex,
                    "dynamic_selection": True,
                },
            )
            state.status = STATE_PAUSED
        else:
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="ENTRY_ERROR",
                level="error",
                message=f"Ошибка входа: {error_code}",
                meta={
                    "symbol": symbol,
                    "buy_exchange": buy_ex,
                    "sell_exchange": sell_ex,
                    "dynamic_selection": True,
                },
            )
            # Статус остаётся READY — в будущем можем попробовать войти ещё раз.


# ==============================
# ENTERING → добор оставшихся частей
# ==============================

async def handle_state_entering(
    db: DBManager,
    market: MarketEngine,
    trader: TradeEngine,
    pair_row: dict,
    state: PairState,
):
    """
    Состояние ENTERING:
      - позиция частично открыта;
      - продолжаем добор частей при сохранении спреда
        по УЖЕ ВЫБРАННОЙ связке бирж (long_exchange / short_exchange);
      - при достижении цели по спреду переключаемся в EXITING.
    """
    if state.is_fully_entered:
        state.status = STATE_HOLD
        return

    if not state.long_exchange or not state.short_exchange:
        logger.warning(f"[{state.pair_id}] ENTERING без заданных бирж long/short")
        state.status = STATE_ERROR
        return

    symbol = state.symbol
    monitor_volume = state.part_volume

    signal = await market.check_spread(
        symbol=symbol,
        buy_exchange=state.long_exchange,
        sell_exchange=state.short_exchange,
        volume_in_coin=monitor_volume,
    )
    if not signal:
        return

    net_spread = signal["net_full_spread_pct"]

    # Если во время добора спред уже ушёл до уровня TP или ниже,
    # не продолжаем добор, а переводим пару сразу в EXITING.
    if net_spread <= state.exit_spread and state.filled_parts > 0:
        logger.info(
            f"[{state.pair_id}] ENTERING → EXITING {symbol} | "
            f"spread={net_spread}% <= exit_target={state.exit_spread}%, "
            f"позиция уже частично набрана ({state.filled_parts}/{state.n_orders})"
        )
        state.status = STATE_EXITING
        return

    # Если спред просел ниже порога входа — добор приостанавливаем
    if net_spread < state.entry_spread:
        logger.debug(
            f"[{state.pair_id}] ENTERING | spread={net_spread}% "
            f"< entry_target={state.entry_spread}% — добор приостановлен"
        )
        return

    # ИСПРАВЛЕНО: передаём pair_id
    res = await trader.execute_entry(signal, monitor_volume, pair_id=state.pair_id)
    
    if res["success"]:
        # Проверяем реально исполненные объёмы
        long_order = res.get("entry_long_order", {})
        short_order = res.get("entry_short_order", {})
        
        filled_long = float(long_order.get("filled") or monitor_volume)
        filled_short = float(short_order.get("filled") or monitor_volume)
        
        state.filled_parts += 1
        state.entry_prices_long.append(signal["buy_price"])
        state.entry_prices_short.append(signal["sell_price"])
        state.actual_long_volume += filled_long
        state.actual_short_volume += filled_short

        # Обновляем позицию в БД после добора
        db.save_position(
            pair_id=state.pair_id,
            long_exchange=state.long_exchange,
            short_exchange=state.short_exchange,
            filled_parts=state.filled_parts,
            closed_parts=state.closed_parts,
            entry_prices_long=state.entry_prices_long,
            entry_prices_short=state.entry_prices_short,
            part_volume=state.part_volume,
        )

        db.log_trade_event(
            pair_id=state.pair_id,
            event_type="ENTRY_OK",
            level="info",
            message=(
                f"Дополнительный вход по {symbol}: "
                f"часть {state.filled_parts}/{state.n_orders}"
            ),
            meta={
                "buy_exchange": state.long_exchange,
                "sell_exchange": state.short_exchange,
                "volume": monitor_volume,
                "filled_long": filled_long,
                "filled_short": filled_short,
                "spread_pct": net_spread,
            },
        )

        if state.is_fully_entered:
            state.status = STATE_HOLD

    else:
        error_code = res.get("error") or "ENTRY_ERROR"
        if error_code == "second_leg_failed_emergency_close":
            db.update_pair_status(state.pair_id, "paused")
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="SECOND_LEG_FAILED",
                level="error",
                message=(
                    "Ошибка второй ноги при доборе позиции, "
                    "LONG закрыт аварийно. Пара поставлена на паузу."
                ),
                meta={"symbol": symbol},
            )
            state.status = STATE_PAUSED
        else:
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="ENTRY_ERROR",
                level="error",
                message=f"Ошибка входа при доборе: {error_code}",
                meta={"symbol": symbol},
            )
            # Позиция частично открыта — дальше только HOLD
            state.status = STATE_HOLD


# ==============================
# HOLD → сопровождение позиции (TP / SL)
# ==============================

async def handle_state_hold(
    db: DBManager,
    market: MarketEngine,
    trader: TradeEngine,
    pair_row: dict,
    state: PairState,
):
    """
    Состояние HOLD:
      - позиция открыта (полностью или частично);
      - считаем PnL;
      - при срабатывании SL закрываем всё;
      - при выполнении TP-условия переводим в EXITING (выход частями).
    """
    if state.open_parts <= 0:
        # На всякий случай чистим позицию в БД, если по логике она уже закрыта
        db.delete_position(state.pair_id)
        state.status = STATE_READY
        return

    if not state.long_exchange or not state.short_exchange:
        logger.warning(f"[{state.pair_id}] HOLD без бирж long/short")
        state.status = STATE_ERROR
        return

    symbol = state.symbol
    open_volume = state.open_volume

    # Защита от деления на ноль - проверяем, что массивы цен не пустые
    if not state.entry_prices_long or not state.entry_prices_short:
        logger.error(
            f"[{state.pair_id}] HOLD: пустые массивы цен входа! "
            f"long={len(state.entry_prices_long)}, short={len(state.entry_prices_short)}"
        )
        # Сбрасываем позицию и переводим в ERROR
        db.delete_position(state.pair_id)
        state.reset_after_exit()
        state.status = STATE_ERROR
        return

    # Средние цены входа
    avg_long_entry = sum(state.entry_prices_long) / len(state.entry_prices_long)
    avg_short_entry = sum(state.entry_prices_short) / len(state.entry_prices_short)

    # Текущие цены выхода (bid на LONG, ask на SHORT)
    pos_prices = await market.get_position_prices(
        symbol=symbol,
        long_exchange=state.long_exchange,
        short_exchange=state.short_exchange,
        volume_in_coin=open_volume,
    )
    if not pos_prices or not pos_prices["valid"]:
        return

    long_exit_price = pos_prices["long_exit_price"]
    short_exit_price = pos_prices["short_exit_price"]

    # PnL (без комиссий, они учтены в спред-логике при входе/выходе)
    pnl_long = (long_exit_price - avg_long_entry) * open_volume
    pnl_short = (avg_short_entry - short_exit_price) * open_volume
    total_pnl = pnl_long + pnl_short

    # SL — закрыть всё сразу
    is_sl = state.stop_loss > 0 and total_pnl <= -state.stop_loss

    # Для TP: проверка спреда на объём одной части
    signal = await market.check_spread(
        symbol=symbol,
        buy_exchange=state.long_exchange,
        sell_exchange=state.short_exchange,
        volume_in_coin=state.part_volume,
    )
    net_spread = signal["net_full_spread_pct"] if signal else None
    is_tp = (net_spread is not None) and (net_spread <= state.exit_spread)

    if is_sl:
        logger.warning(
            f"[{state.pair_id}] SL TRIGGERED {symbol} | "
            f"PnL={total_pnl:.2f}$ <= -{state.stop_loss}$"
        )

        # ИСПРАВЛЕНО: добавлен pair_id в position_info
        position_info = {
            "symbol": symbol,
            "long_exchange": state.long_exchange,
            "short_exchange": state.short_exchange,
            "pair_id": state.pair_id,
        }
        res = await trader.execute_exit(position_info, open_volume)

        if res["success"]:
            db.update_pair_pnl(state.pair_id, total_pnl)
            db.increment_sl(state.pair_id)
            db.update_pair_status(state.pair_id, "paused")
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="SL_TRIGGERED",
                level="error",
                message=(
                    f"SL по {symbol}: PnL={total_pnl:.2f}$, "
                    f"пара поставлена на паузу."
                ),
                meta={
                    "pnl": total_pnl,
                    "stop_loss": state.stop_loss,
                    "long_exchange": state.long_exchange,
                    "short_exchange": state.short_exchange,
                },
            )
            # Позиция полностью закрыта — чистим запись в positions
            db.delete_position(state.pair_id)
            state.reset_after_exit()
            state.status = STATE_PAUSED
        else:
            db.log_trade_event(
                pair_id=state.pair_id,
                event_type="EXIT_ERROR",
                level="error",
                message=f"Не удалось закрыть позицию по SL: {res.get('error')}",
                meta={"symbol": symbol},
            )
        return

    # TP: если условие выполнено — переходим в EXITING, выходим частями
    if is_tp:
        logger.info(
            f"[{state.pair_id}] TP CONDITION {symbol} | "
            f"spread={net_spread}% <= exit_target={state.exit_spread}%"
        )
        state.status = STATE_EXITING
    # иначе просто продолжаем HOLD


# ==============================
# EXITING → частичный выход
# ==============================

async def handle_state_exiting(
    db: DBManager,
    market: MarketEngine,
    trader: TradeEngine,
    pair_row: dict,
    state: PairState,
):
    """
    Состояние EXITING:
      - позиция закрывается частями;
      - каждая часть выходит при выполнении условий по спреду;
      - после закрытия всех частей вызывается finalize_full_exit().
    """
    if state.open_parts <= 0:
        await finalize_full_exit(db, market, state)
        return

    if not state.long_exchange or not state.short_exchange:
        logger.warning(f"[{state.pair_id}] EXITING без бирж long/short")
        state.status = STATE_ERROR
        return

    symbol = state.symbol
    volume_to_close = state.part_volume

    # Проверяем, сохраняется ли условие спреда выхода для очередной части
    signal = await market.check_spread(
        symbol=symbol,
        buy_exchange=state.long_exchange,
        sell_exchange=state.short_exchange,
        volume_in_coin=volume_to_close,
    )
    if not signal:
        return

    net_spread = signal["net_full_spread_pct"]

    if net_spread > state.exit_spread:
        logger.debug(
            f"[{state.pair_id}] EXITING | "
            f"spread={net_spread}% > exit_target={state.exit_spread}% — ждём"
        )
        return

    # Условие выхода выполнено — закрываем ОДНУ часть
    # ИСПРАВЛЕНО: добавлен pair_id в position_info
    position_info = {
        "symbol": symbol,
        "long_exchange": state.long_exchange,
        "short_exchange": state.short_exchange,
        "pair_id": state.pair_id,
    }
    res = await trader.execute_exit(position_info, volume_to_close)

    if res["success"]:
        state.closed_parts += 1

        # ИСПРАВЛЕНИЕ баг #9: Корректируем фактические объёмы при частичном выходе
        if state.actual_long_volume > 0:
            state.actual_long_volume = max(0.0, state.actual_long_volume - volume_to_close)
        if state.actual_short_volume > 0:
            state.actual_short_volume = max(0.0, state.actual_short_volume - volume_to_close)

        # Для оценки итогового PnL сохраняем текущие цены выхода
        pos_prices = await market.get_position_prices(
            symbol=symbol,
            long_exchange=state.long_exchange,
            short_exchange=state.short_exchange,
            volume_in_coin=volume_to_close,
        )
        if pos_prices and pos_prices["valid"]:
            state.exit_prices_long.append(pos_prices["long_exit_price"])
            state.exit_prices_short.append(pos_prices["short_exit_price"])

        # Обновляем запись в positions (closed_parts увеличился)
        db.save_position(
            pair_id=state.pair_id,
            long_exchange=state.long_exchange,
            short_exchange=state.short_exchange,
            filled_parts=state.filled_parts,
            closed_parts=state.closed_parts,
            entry_prices_long=state.entry_prices_long,
            entry_prices_short=state.entry_prices_short,
            part_volume=state.part_volume,
        )

        db.log_trade_event(
            pair_id=state.pair_id,
            event_type="EXIT_PART_OK",
            level="info",
            message=(
                f"Частичный выход по {symbol}: "
                f"часть {state.closed_parts}/{state.filled_parts}"
            ),
            meta={
                "volume_closed": volume_to_close,
                "spread_pct": net_spread,
            },
        )

        if state.open_parts <= 0:
            await finalize_full_exit(db, market, state)

    else:
        db.log_trade_event(
            pair_id=state.pair_id,
            event_type="EXIT_ERROR",
            level="error",
            message=f"Ошибка частичного выхода: {res.get('error')}",
            meta={"symbol": symbol},
        )
        # Стратегия: оставляем статус EXITING и пробуем дальше.


# ==============================
# Финализация полного выхода (TP-сценарий)
# ==============================

async def finalize_full_exit(db: DBManager, market: MarketEngine, state: PairState):
    """
    Когда позиция закрыта полностью (по TP частями),
    считаем итоговый PnL и обновляем БД.
    """
    if state.filled_parts <= 0:
        state.reset_after_exit()
        state.status = STATE_READY
        return

    total_volume = state.part_volume * state.filled_parts

    # Нормальный путь: есть накопленные цены выхода по частям
    if state.exit_prices_long and state.exit_prices_short:
        avg_long_exit = sum(state.exit_prices_long) / len(state.exit_prices_long)
        avg_short_exit = sum(state.exit_prices_short) / len(state.exit_prices_short)
    else:
        # Если exit-цены не были накоплены (например, WS лагал),
        # пробуем взять рыночные цены закрытия позиции.
        logger.warning(
            f"[{state.pair_id}] finalize_full_exit: нет накопленных exit-цен, "
            f"пробуем взять текущие рыночные цены."
        )
        pos_prices = await market.get_position_prices(
            symbol=state.symbol,
            long_exchange=state.long_exchange,
            short_exchange=state.short_exchange,
            volume_in_coin=total_volume,
        )
        if pos_prices and pos_prices.get("valid"):
            avg_long_exit = pos_prices["long_exit_price"]
            avg_short_exit = pos_prices["short_exit_price"]
        else:
            # Фоллбек: используем входные цены (хуже, чем ничего, но не падаем).
            logger.error(
                f"[{state.pair_id}] finalize_full_exit: не удалось получить рыночные "
                f"цены выхода, используем входные цены как фоллбек."
            )
            avg_long_exit = sum(state.entry_prices_long) / len(state.entry_prices_long)
            avg_short_exit = sum(state.entry_prices_short) / len(state.entry_prices_short)

    avg_long_entry = sum(state.entry_prices_long) / len(state.entry_prices_long)
    avg_short_entry = sum(state.entry_prices_short) / len(state.entry_prices_short)

    pnl_long = (avg_long_exit - avg_long_entry) * total_volume
    pnl_short = (avg_short_entry - avg_short_exit) * total_volume
    total_pnl = pnl_long + pnl_short

    db.update_pair_pnl(state.pair_id, total_pnl)
    db.log_trade_event(
        pair_id=state.pair_id,
        event_type="EXIT_OK",
        level="info",
        message=f"Полный выход по паре, PnL={total_pnl:.2f}$",
        meta={
            "pnl": total_pnl,
            "total_volume": total_volume,
        },
    )

    # После полного выхода по TP позиция больше не нужна в positions
    db.delete_position(state.pair_id)

    logger.success(
        f"[{state.pair_id}] ✔ EXIT COMPLETED | PnL={total_pnl:.2f}$, пара возвращена в READY"
    )

    state.reset_after_exit()
    state.status = STATE_READY


# Для локального запуска (без launcher.py)
if __name__ == "__main__":
    asyncio.run(main())
