# market_engine.py
# ---------------------------------------------------
# MarketEngine — слой рыночной логики:
#   • читает стаканы из WsManager (thread-safe)
#   • считает VWAP c защитой по ликвидности
#   • считает спред между биржами (в обе стороны buy/sell)
#   • ищет ЛУЧШУЮ связку бирж и направление
#   • отдаёт цены выхода для PnL
#   • учитывает per-exchange комиссии из config.py
#   • кэширует результаты для оптимизации O(N²)
# ---------------------------------------------------

import asyncio
import time
from typing import List, Dict, Optional, Tuple
from dataclasses import dataclass, field
from loguru import logger

# Импортируем комиссии и параметры из config.py
from config import DEAD_WS_TIMEOUT, TAKER_FEES, DEFAULT_TAKER_FEE


# ============================================================
# КОНФИГУРАЦИЯ
# ============================================================

# Время жизни кэша спреда (секунды)
SPREAD_CACHE_TTL = 0.9  # 900ms — 90% от тика в 1 секунду для максимальной эффективности

# Минимальный спред для раннего выхода из перебора (%)
EARLY_EXIT_SPREAD_THRESHOLD = 1.0  # Если нашли спред >= 1%, можно не искать дальше

# Максимальное количество параллельных проверок
MAX_PARALLEL_CHECKS = 10


# ============================================================
# КЭШИРОВАНИЕ СПРЕДОВ
# ============================================================

@dataclass
class CachedSpread:
    """Закэшированный результат расчёта спреда."""
    data: Optional[Dict]
    timestamp: float
    
    def is_valid(self, ttl: float = SPREAD_CACHE_TTL) -> bool:
        """Проверить, не устарел ли кэш."""
        return (time.time() - self.timestamp) < ttl


@dataclass
class MarketMetrics:
    """Метрики работы MarketEngine для мониторинга."""
    spreads_calculated: int = 0
    cache_hits: int = 0
    cache_misses: int = 0
    opportunities_found: int = 0
    last_scan_duration_ms: float = 0.0
    last_scan_pairs_checked: int = 0
    
    def record_scan(self, duration_ms: float, pairs_checked: int):
        """Записать результаты сканирования."""
        self.last_scan_duration_ms = duration_ms
        self.last_scan_pairs_checked = pairs_checked
    
    def to_dict(self) -> dict:
        """Для логирования."""
        cache_total = self.cache_hits + self.cache_misses
        hit_rate = (self.cache_hits / cache_total * 100) if cache_total > 0 else 0
        return {
            "spreads_calculated": self.spreads_calculated,
            "cache_hit_rate": f"{hit_rate:.1f}%",
            "opportunities_found": self.opportunities_found,
            "last_scan_ms": round(self.last_scan_duration_ms, 2),
            "last_scan_pairs": self.last_scan_pairs_checked,
        }


class MarketEngine:
    """
    MarketEngine инкапсулирует рыночную логику вокруг WsManager:

      - безопасный VWAP (проверка ликвидности, защита от мусорных уровней),
      - проверка спреда между двумя биржами с учётом комиссий,
      - перебор всех бирж + обоих направлений (buy/sell) для поиска
        лучшей возможности,
      - получение корректных цен закрытия позиции для PnL,
      - кэширование спредов для оптимизации.
    """

    def __init__(self, ws_manager):
        self.ws = ws_manager
        
        # Кэш спредов: (buy_ex, sell_ex, symbol) -> CachedSpread
        self._spread_cache: Dict[Tuple[str, str, str], CachedSpread] = {}
        
        # Метрики
        self.metrics = MarketMetrics()

    # ============================================================
    # ВСПОМОГАТЕЛЬНОЕ: комиссии и стаканы
    # ============================================================

    def _normalize_exchange(self, name: str) -> str:
        """Приводим название биржи к единому виду."""
        return (name or "").strip().lower()

    def _get_taker_fee(self, exchange: str) -> float:
        """
        Возвращает тейкерскую комиссию для биржи в %.
        Если биржа не найдена — используем DEFAULT_TAKER_FEE.
        """
        ex = self._normalize_exchange(exchange)
        return float(TAKER_FEES.get(ex, DEFAULT_TAKER_FEE))

    def _get_orderbook(self, exchange: str, symbol: str) -> Optional[Dict]:
        """
        Берём последний известный стакан из WsManager.
        WsManager возвращает копию — thread-safe.
        """
        ex = self._normalize_exchange(exchange)
        return self.ws.get_latest_book(ex, symbol)

    def _is_book_fresh(self, ob: Dict) -> bool:
        """Проверка свежести стакана."""
        if not ob:
            return False

        ts = ob.get("timestamp", 0)
        if not isinstance(ts, (int, float)):
            return False

        age = time.time() - ts
        if age > DEAD_WS_TIMEOUT:
            logger.debug(f"[STALE] стакан протух: age={age:.2f}s > {DEAD_WS_TIMEOUT}s")
            return False

        return True

    # ============================================================
    # УПРАВЛЕНИЕ КЭШЕМ
    # ============================================================

    def _get_cached_spread(
        self, 
        buy_ex: str, 
        sell_ex: str, 
        symbol: str
    ) -> Optional[Dict]:
        """Получить спред из кэша, если он ещё валиден."""
        key = (buy_ex, sell_ex, symbol)
        cached = self._spread_cache.get(key)
        
        if cached and cached.is_valid():
            self.metrics.cache_hits += 1
            return cached.data
        
        self.metrics.cache_misses += 1
        return None

    def _set_cached_spread(
        self, 
        buy_ex: str, 
        sell_ex: str, 
        symbol: str, 
        data: Optional[Dict]
    ):
        """Сохранить спред в кэш."""
        key = (buy_ex, sell_ex, symbol)
        self._spread_cache[key] = CachedSpread(
            data=data,
            timestamp=time.time()
        )

    def clear_cache(self):
        """Очистить весь кэш спредов."""
        self._spread_cache.clear()

    def cleanup_stale_cache(self):
        """Удалить устаревшие записи из кэша."""
        now = time.time()
        stale_keys = [
            key for key, cached in self._spread_cache.items()
            if not cached.is_valid()
        ]
        for key in stale_keys:
            del self._spread_cache[key]

    # ============================================================
    # VWAP (усиленный, защищённый)
    # ============================================================

    def calculate_vwap(
        self, 
        levels: List[List[float]], 
        target_volume: float,
        min_fill_ratio: float = 0.98
    ) -> Optional[float]:
        """
        Усовершенствованный VWAP с проверкой ликвидности.
        
        Args:
            levels: Уровни стакана [[price, qty], ...]
            target_volume: Целевой объём
            min_fill_ratio: Минимальная доля заполнения (0.98 = 98%)
        
        Returns:
            VWAP цена или None если недостаточно ликвидности
        """
        if not levels or target_volume <= 0:
            return None

        total_cost = 0.0
        collected = 0.0

        for level in levels:
            try:
                price = float(level[0])
                qty = float(level[1])
            except (IndexError, TypeError, ValueError):
                continue

            if price <= 0 or qty <= 0:
                continue

            need = target_volume - collected
            if need <= 0:
                break

            take = min(qty, need)
            total_cost += take * price
            collected += take

            if collected >= target_volume:
                break

        if collected < target_volume * min_fill_ratio:
            logger.debug(
                f"[VWAP] Недостаточно ликвидности: need={target_volume}, "
                f"have={collected} ({collected/target_volume*100:.1f}%)"
            )
            return None

        return total_cost / collected

    def _get_side_price(
        self, 
        orderbook: Dict, 
        side: str, 
        volume: float
    ) -> Optional[float]:
        """VWAP по нужной стороне стакана."""
        if not orderbook or volume <= 0:
            return None

        if side == "buy":
            levels = orderbook.get("asks", [])
        else:
            levels = orderbook.get("bids", [])

        if not levels:
            return None

        return self.calculate_vwap(levels, volume)

    # ============================================================
    # Расчёт спреда между ДВУМЯ биржами
    # ============================================================

    async def check_spread(
        self,
        symbol: str,
        buy_exchange: str,
        sell_exchange: str,
        volume_in_coin: float,
        use_cache: bool = True,
    ) -> Optional[Dict]:
        """
        Расчёт спреда между двумя биржами в конкретном направлении.
        
        Args:
            symbol: Торговая пара
            buy_exchange: Биржа для покупки (LONG)
            sell_exchange: Биржа для продажи (SHORT)
            volume_in_coin: Объём в базовой валюте
            use_cache: Использовать кэш (default: True)
        """
        if volume_in_coin <= 0:
            return None

        be = self._normalize_exchange(buy_exchange)
        se = self._normalize_exchange(sell_exchange)
        
        if be == se:
            return None

        # Проверяем кэш
        if use_cache:
            cached = self._get_cached_spread(be, se, symbol)
            if cached is not None:
                return cached

        # Получаем стаканы (WsManager возвращает копии — thread-safe)
        ob_buy = self._get_orderbook(be, symbol)
        ob_sell = self._get_orderbook(se, symbol)

        if not ob_buy or not ob_sell:
            self._set_cached_spread(be, se, symbol, None)
            return None
            
        if not self._is_book_fresh(ob_buy) or not self._is_book_fresh(ob_sell):
            self._set_cached_spread(be, se, symbol, None)
            return None

        price_buy = self._get_side_price(ob_buy, "buy", volume_in_coin)
        price_sell = self._get_side_price(ob_sell, "sell", volume_in_coin)

        if price_buy is None or price_sell is None:
            self._set_cached_spread(be, se, symbol, None)
            return None

        # Аномалия: цена продажи ниже цены покупки
        if price_sell < price_buy:
            logger.debug(
                f"[ANOMALY {symbol}] {be}->{se} | "
                f"sell({price_sell:.6f}) < buy({price_buy:.6f})"
            )
            self._set_cached_spread(be, se, symbol, None)
            return None

        # Расчёт спредов
        raw_spread_pct = (price_sell - price_buy) / price_buy * 100.0

        fee_buy = self._get_taker_fee(be)
        fee_sell = self._get_taker_fee(se)

        entry_fees = fee_buy + fee_sell
        full_cycle_fees = entry_fees * 2

        net_entry = raw_spread_pct - entry_fees
        net_full = raw_spread_pct - full_cycle_fees

        result = {
            "valid": True,
            "symbol": symbol,
            "volume": volume_in_coin,
            "buy_exchange": be,
            "sell_exchange": se,
            "buy_price": price_buy,
            "sell_price": price_sell,
            "raw_spread_pct": round(raw_spread_pct, 4),
            "net_entry_spread_pct": round(net_entry, 4),
            "net_full_spread_pct": round(net_full, 4),
            "fee_buy_pct": fee_buy,
            "fee_sell_pct": fee_sell,
            "timestamp": min(ob_buy["timestamp"], ob_sell["timestamp"]),
            "spread_pct": round(net_full, 4),  # алиас для совместимости
        }

        # Сохраняем в кэш
        self._set_cached_spread(be, se, symbol, result)
        self.metrics.spreads_calculated += 1

        return result

    # ============================================================
    # Поиск лучшего направления и связки бирж (ОПТИМИЗИРОВАННЫЙ)
    # ============================================================

    async def find_best_opportunity(
        self,
        symbol: str,
        volume_in_coin: float,
        exchanges: Optional[List[str]] = None,
        min_spread_pct: float = 0.0,
        early_exit: bool = True,
    ) -> Optional[Dict]:
        """
        Найти лучшую арбитражную возможность O(N) алгоритм.

        Оптимизация:
        - Один проход по биржам для поиска лучших цен покупки/продажи
        - Один вызов check_spread для найденной пары
        - Сложность O(N) вместо O(N²)

        Args:
            symbol: Торговая пара
            volume_in_coin: Объём в базовой валюте
            exchanges: Список бирж (или все из TAKER_FEES)
            min_spread_pct: Минимальный порог спреда
            early_exit: Не используется (для обратной совместимости)
        """
        if volume_in_coin <= 0:
            return None

        start_time = time.time()

        if exchanges:
            ex_list = [self._normalize_exchange(e) for e in exchanges]
        else:
            ex_list = list(TAKER_FEES.keys())

        if len(ex_list) < 2:
            return None

        # ШАГ 1: Найти лучшую цену покупки и лучшую цену продажи - O(N)
        best_buy_price = float('inf')
        best_buy_exchange = None
        best_sell_price = 0.0
        best_sell_exchange = None

        for ex in ex_list:
            # Получаем стакан
            ob = self._get_orderbook(ex, symbol)
            if not ob or not self._is_book_fresh(ob):
                continue

            # Для покупки (LONG) нам нужны asks - мы покупаем по ask
            buy_price = self._get_side_price(ob, "buy", volume_in_coin)
            if buy_price and buy_price < best_buy_price:
                best_buy_price = buy_price
                best_buy_exchange = ex

            # Для продажи (SHORT) нам нужны bids - мы продаём по bid
            sell_price = self._get_side_price(ob, "sell", volume_in_coin)
            if sell_price and sell_price > best_sell_price:
                best_sell_price = sell_price
                best_sell_exchange = ex

        # ШАГ 2: Если не нашли обе биржи или они совпадают - выход
        if not best_buy_exchange or not best_sell_exchange:
            duration_ms = (time.time() - start_time) * 1000
            self.metrics.record_scan(duration_ms, len(ex_list))
            return None

        if best_buy_exchange == best_sell_exchange:
            duration_ms = (time.time() - start_time) * 1000
            self.metrics.record_scan(duration_ms, len(ex_list))
            logger.debug(
                f"[{symbol}] Лучшие цены на одной бирже {best_buy_exchange}, арбитраж невозможен"
            )
            return None

        # ШАГ 3: Проверка аномалии (цена продажи ниже цены покупки)
        if best_sell_price < best_buy_price:
            duration_ms = (time.time() - start_time) * 1000
            self.metrics.record_scan(duration_ms, len(ex_list))
            logger.debug(
                f"[ANOMALY {symbol}] sell_price({best_sell_price:.6f}) < buy_price({best_buy_price:.6f})"
            )
            return None

        # ШАГ 4: Рассчитать спред для найденной пары - всего 1 вызов!
        result = await self.check_spread(
            symbol=symbol,
            buy_exchange=best_buy_exchange,
            sell_exchange=best_sell_exchange,
            volume_in_coin=volume_in_coin,
            use_cache=False,  # Стаканы только что получены, кэш не нужен
        )

        # Записываем метрики
        duration_ms = (time.time() - start_time) * 1000
        self.metrics.record_scan(duration_ms, len(ex_list))

        # Проверяем порог спреда
        if result and result["net_full_spread_pct"] >= min_spread_pct:
            self.metrics.opportunities_found += 1
            logger.info(
                f"[BEST {symbol}] {result['buy_exchange']}->{result['sell_exchange']} | "
                f"net_full={result['net_full_spread_pct']}% (>= {min_spread_pct}%) | "
                f"scan={duration_ms:.1f}ms | O(N) алгоритм"
            )
            return result

        return None

    # ============================================================
    # Быстрая проверка: есть ли вообще возможность
    # ============================================================

    async def has_opportunity(
        self,
        symbol: str,
        volume_in_coin: float,
        exchanges: Optional[List[str]] = None,
        min_spread_pct: float = 0.0,
    ) -> bool:
        """
        Быстрая проверка наличия арбитражной возможности.
        Останавливается при первом найденном спреде >= min_spread_pct.
        """
        if volume_in_coin <= 0:
            return False

        if exchanges:
            ex_list = [self._normalize_exchange(e) for e in exchanges]
        else:
            ex_list = list(TAKER_FEES.keys())

        if len(ex_list) < 2:
            return False

        for be in ex_list:
            for se in ex_list:
                if be == se:
                    continue
                    
                sig = await self.check_spread(symbol, be, se, volume_in_coin)
                if sig and sig["net_full_spread_pct"] >= min_spread_pct:
                    return True

        return False

    # ============================================================
    # Цены выхода позиции (PnL)
    # ============================================================

    async def get_position_prices(
        self,
        symbol: str,
        long_exchange: str,
        short_exchange: str,
        volume_in_coin: float,
    ) -> Optional[Dict]:
        """
        Получить текущие цены для оценки PnL позиции.
        
        Для LONG позиции: цена выхода = bid (продаём)
        Для SHORT позиции: цена выхода = ask (покупаем для закрытия)
        """
        if volume_in_coin <= 0:
            return None

        le = self._normalize_exchange(long_exchange)
        se = self._normalize_exchange(short_exchange)
        
        if le == se:
            return None

        ob_long = self._get_orderbook(le, symbol)
        ob_short = self._get_orderbook(se, symbol)

        if not ob_long or not ob_short:
            return None
            
        if not self._is_book_fresh(ob_long) or not self._is_book_fresh(ob_short):
            return None

        # LONG exit = продаём = смотрим bids
        long_exit = self._get_side_price(ob_long, "sell", volume_in_coin)
        # SHORT exit = покупаем = смотрим asks
        short_exit = self._get_side_price(ob_short, "buy", volume_in_coin)

        if long_exit is None or short_exit is None:
            return None

        return {
            "valid": True,
            "symbol": symbol,
            "long_exchange": le,
            "short_exchange": se,
            "long_exit_price": long_exit,
            "short_exit_price": short_exit,
            "timestamp": min(ob_long["timestamp"], ob_short["timestamp"]),
        }

    # ============================================================
    # Оценка текущего PnL позиции
    # ============================================================

    async def estimate_position_pnl(
        self,
        symbol: str,
        long_exchange: str,
        short_exchange: str,
        volume_in_coin: float,
        avg_entry_long: float,
        avg_entry_short: float,
    ) -> Optional[Dict]:
        """
        Оценить текущий PnL открытой позиции.
        
        Returns:
            {
                "pnl_long": float,
                "pnl_short": float,
                "total_pnl": float,
                "current_spread_pct": float,
                ...
            }
        """
        prices = await self.get_position_prices(
            symbol, long_exchange, short_exchange, volume_in_coin
        )
        
        if not prices or not prices["valid"]:
            return None

        long_exit = prices["long_exit_price"]
        short_exit = prices["short_exit_price"]

        pnl_long = (long_exit - avg_entry_long) * volume_in_coin
        pnl_short = (avg_entry_short - short_exit) * volume_in_coin
        total_pnl = pnl_long + pnl_short

        # Текущий спред (для мониторинга)
        current_spread = (avg_entry_short - avg_entry_long) - (short_exit - long_exit)
        current_spread_pct = current_spread / avg_entry_long * 100 if avg_entry_long > 0 else 0

        return {
            "valid": True,
            "symbol": symbol,
            "long_exchange": long_exchange,
            "short_exchange": short_exchange,
            "volume": volume_in_coin,
            "avg_entry_long": avg_entry_long,
            "avg_entry_short": avg_entry_short,
            "current_exit_long": long_exit,
            "current_exit_short": short_exit,
            "pnl_long": round(pnl_long, 4),
            "pnl_short": round(pnl_short, 4),
            "total_pnl": round(total_pnl, 4),
            "current_spread_pct": round(current_spread_pct, 4),
            "timestamp": prices["timestamp"],
        }

    # ============================================================
    # МЕТРИКИ
    # ============================================================

    def get_metrics(self) -> dict:
        """Получить метрики работы MarketEngine."""
        return self.metrics.to_dict()
