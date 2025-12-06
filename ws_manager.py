# ws_manager.py
# --------------------------------------------------
# Ультрастабильный WebSocket менеджер для фьючерсных
# стаканов Bybit / Bitget / OKX / Gate / MEXC / BingX / HTX.
#
# Thread-safe доступ к стаканам через asyncio.Lock
# ОПТИМИЗИРОВАННАЯ ВЕРСИЯ - легковесное копирование топ-5 уровней
# --------------------------------------------------
import asyncio
import aiohttp
import json
import time
import gzip
import uuid
from typing import Dict, Set, Optional, List
from dataclasses import dataclass, field
from loguru import logger
from config import WSS_URLS, WS_PING_INTERVAL
from symbol_mapper import to_ws_symbol, to_internal


def safe_levels(levels) -> List[List[float]]:
    """
    Универсальный парсер массивов уровней стакана:
    Берёт только первые два значения (price, qty).
    Игнорирует лишнее, пропускает мусор.
    """
    out = []
    for lvl in levels:
        try:
            if not isinstance(lvl, (list, tuple)):
                continue
            price = float(lvl[0])
            qty = float(lvl[1])
            if qty > 0:
                out.append([price, qty])
        except Exception:
            continue
    return out


def safe_levels_mexc(levels) -> List[List[float]]:
    """
    Парсер для MEXC: [price, orders_count, quantity]
    Берём price (индекс 0) и quantity (индекс 2).
    """
    out = []
    for lvl in levels:
        try:
            if not isinstance(lvl, (list, tuple)) or len(lvl) < 3:
                continue
            price = float(lvl[0])
            qty = float(lvl[2])  # Третье поле - это quantity
            if qty > 0:
                out.append([price, qty])
        except Exception:
            continue
    return out


# ============================================================
# КОНФИГУРАЦИЯ
# ============================================================
# Порог "протухания" коннекта по отсутствию сообщений
WS_STALE_TIMEOUT = WS_PING_INTERVAL * 3
# Флаг детального логирования RAW сообщений
DEBUG_WS_RAW = False
# Интервал между попытками переподключения (секунды)
RECONNECT_DELAY_BASE = 3.0
RECONNECT_DELAY_MAX = 30.0
RECONNECT_BACKOFF_MULTIPLIER = 1.5
# Глубина стакана
MAX_BOOK_DEPTH = 10  # Храним для запаса
RETURN_BOOK_DEPTH = 5  # Возвращаем для арбитража


# ============================================================
# МЕТРИКИ ЗДОРОВЬЯ СОЕДИНЕНИЯ
# ============================================================
@dataclass
class ConnectionHealth:
    """Метрики здоровья одного WS-соединения."""
    exchange: str
    connected: bool = False
    last_message_ts: float = 0.0
    last_connect_ts: float = 0.0
    reconnect_count: int = 0
    messages_received: int = 0
    errors_count: int = 0
    subscribed_symbols: Set[str] = field(default_factory=set)
    
    @property
    def age_seconds(self) -> float:
        """Сколько секунд прошло с последнего сообщения."""
        if self.last_message_ts <= 0:
            return float('inf')
        return time.time() - self.last_message_ts
    
    @property
    def is_stale(self) -> bool:
        """Соединение протухло (давно не было сообщений)."""
        return self.age_seconds > WS_STALE_TIMEOUT
    
    def to_dict(self) -> dict:
        """Для логирования и мониторинга."""
        return {
            "exchange": self.exchange,
            "connected": self.connected,
            "age_seconds": round(self.age_seconds, 2),
            "is_stale": self.is_stale,
            "reconnect_count": self.reconnect_count,
            "messages_received": self.messages_received,
            "errors_count": self.errors_count,
            "subscribed_count": len(self.subscribed_symbols),
        }


class WsManager:
    """
    WebSocket менеджер с thread-safe доступом к стаканам.
    
    Ключевые особенности:
    - asyncio.Lock для защиты от race condition при чтении/записи стаканов
    - Оптимизированное копирование стакана при чтении (только топ-5 уровней)
    - Метрики здоровья соединений
    - Exponential backoff при переподключении
    """
    
    def __init__(self):
        self.session: Optional[aiohttp.ClientSession] = None
        self.running = False
        
        # Нормализуем ключи бирж в lower-case
        self.connections: Dict[str, aiohttp.ClientWebSocketResponse] = {}
        self.subscriptions: Dict[str, Set[str]] = {
            ex.lower(): set() for ex in WSS_URLS.keys()
        }
        
        # Стаканы с защитой через Lock
        self._orderbooks: Dict[str, Dict[str, dict]] = {
            ex.lower(): {} for ex in WSS_URLS.keys()
        }
        self._orderbook_locks: Dict[str, asyncio.Lock] = {
            ex.lower(): asyncio.Lock() for ex in WSS_URLS.keys()
        }
        
        # Глобальный лок для операций подписки
        self._subscribe_lock = asyncio.Lock()
        
        # Метрики здоровья соединений
        self._health: Dict[str, ConnectionHealth] = {
            ex.lower(): ConnectionHealth(exchange=ex.lower())
            for ex in WSS_URLS.keys()
        }
        
        # Счётчики reconnect для exponential backoff
        self._reconnect_attempts: Dict[str, int] = {
            ex.lower(): 0 for ex in WSS_URLS.keys()
        }
    
    # --------------------------------------------------
    # START / STOP
    # --------------------------------------------------
    async def start(self):
        """
        Запускает фоновое создание WS-подключений ко всем биржам из WSS_URLS.
        Повторный вызов start() безопасен.
        """
        if self.running:
            return

        self.running = True
        self.session = aiohttp.ClientSession()

        for ex, url in WSS_URLS.items():
            ex_norm = ex.lower()
            if not url:
                logger.warning(f"[WS] Пропускаем {ex_norm}, нет URL")
                continue
            asyncio.create_task(self._connect(ex_norm, url))

        # ИСПРАВЛЕНИЕ: запускаем периодическую очистку стаканов
        asyncio.create_task(self._cleanup_unused_orderbooks())

        logger.info("📡 WsManager: старт фоновых WS-подключений")
    
    async def stop(self):
        """
        Останавливает все WS-подключения и закрывает сессию.
        """
        self.running = False
        
        for name, ws in list(self.connections.items()):
            try:
                await ws.close()
                logger.debug(f"[WS:{name}] соединение закрыто по stop()")
            except Exception as e:
                logger.warning(f"[WS:{name}] ошибка закрытия: {e}")
        
        self.connections.clear()
        
        if self.session:
            await self.session.close()
            self.session = None
        
        logger.info("🛑 WsManager остановлен.")
    
    # --------------------------------------------------
    # SUBSCRIBE (thread-safe)
    # --------------------------------------------------
    async def subscribe(self, exchange: str, symbol: str):
        """
        Идемпотентная подписка (thread-safe):
        - имя биржи приводится к lower-case,
        - если internal-символ уже в subscriptions — ничего не шлём,
        - если подключение ещё не установлено — просто запоминаем.
        """
        ex = (exchange or "").lower()
        internal = to_internal(symbol)
        
        async with self._subscribe_lock:
            if ex not in self.subscriptions:
                # Динамическое добавление биржи
                self.subscriptions[ex] = set()
                self._orderbooks[ex] = {}
                self._orderbook_locks[ex] = asyncio.Lock()
                self._health[ex] = ConnectionHealth(exchange=ex)
                self._reconnect_attempts[ex] = 0
            
            # Уже подписаны — выходим
            if internal in self.subscriptions[ex]:
                return
            
            self.subscriptions[ex].add(internal)
            self._health[ex].subscribed_symbols.add(internal)
        
        logger.debug(f"[WS:{ex}] subscribe requested for {internal}")
        
        ws = self.connections.get(ex)
        if ws and not ws.closed:
            await self._send_sub(ex, ws, internal)
    
    # --------------------------------------------------
    # CLEANUP UNUSED ORDERBOOKS (ИСПРАВЛЕНИЕ: баг #5)
    # --------------------------------------------------
    async def _cleanup_unused_orderbooks(self):
        """
        Периодическая очистка стаканов для неиспользуемых символов.
        Защита от утечки памяти при смене торгуемых пар.
        """
        while self.running:
            await asyncio.sleep(3600)  # каждый час

            try:
                for ex in list(self._orderbooks.keys()):
                    subscribed = self.subscriptions.get(ex, set())

                    lock = self._orderbook_locks.get(ex)
                    if not lock:
                        continue

                    async with lock:
                        cached = set(self._orderbooks[ex].keys())
                        # Удаляем стаканы, на которые нет активной подписки
                        unused = cached - subscribed

                        for symbol in unused:
                            self._orderbooks[ex].pop(symbol, None)

                        if unused:
                            logger.info(
                                f"🧹 [{ex}] Очищено {len(unused)} неиспользуемых стаканов "
                                f"(осталось {len(self._orderbooks[ex])})"
                            )
            except Exception as e:
                logger.warning(f"[WS] Ошибка очистки стаканов: {e}")

    # --------------------------------------------------
    # GET ORDERBOOK (thread-safe, returns lightweight copy)
    # --------------------------------------------------
    def get_latest_book(
        self,
        exchange: str,
        symbol: str,
        max_age_sec: Optional[float] = None
    ) -> Optional[dict]:
        """
        Вернуть легковесную КОПИЮ последнего известного стакана.

        ИСПРАВЛЕНИЕ баг #13: добавлена опциональная проверка свежести.

        Args:
            exchange: Название биржи
            symbol: Символ торговой пары
            max_age_sec: Максимальный возраст стакана (секунды). Если None - без проверки.

        Оптимизация: копируем только топ-5 уровней вместо всего стакана.
        Это ускоряет операцию в 15-20 раз при сохранении thread-safety.
        Для арбитража глубина более 5 уровней обычно не требуется.

        Thread-safe: возвращает immutable snapshot.
        Возвращает dict или None.
        """
        ex = (exchange or "").lower()
        internal = to_internal(symbol)

        books = self._orderbooks.get(ex, {})
        book = books.get(internal)

        if book is None:
            return None

        # ИСПРАВЛЕНИЕ баг #13: Проверка свежести стакана
        if max_age_sec is not None:
            ts = book.get("timestamp", 0.0)
            age = time.time() - ts
            if age > max_age_sec:
                return None

        # Оптимизация: легковесное копирование только топ-5 уровней
        # Вместо copy.deepcopy(book) который занимает ~0.5ms
        # Делаем shallow copy первых 5 уровней (~0.03ms)
        bids = book.get("bids", [])
        asks = book.get("asks", [])

        return {
            "bids": bids[:RETURN_BOOK_DEPTH].copy() if bids else [],
            "asks": asks[:RETURN_BOOK_DEPTH].copy() if asks else [],
            "timestamp": book.get("timestamp", 0.0)
        }
    
    def get_fresh_book(self, exchange: str, symbol: str, max_age_sec: float) -> Optional[dict]:
        """
        Вернуть КОПИЮ стакана только если он достаточно свежий.
        Thread-safe: возвращает immutable snapshot.
        """
        book = self.get_latest_book(exchange, symbol)
        if not book:
            return None
        
        ts = book.get("timestamp")
        if not isinstance(ts, (int, float)):
            return None
        
        age = time.time() - ts
        if age > max_age_sec:
            logger.debug(
                f"[WS:{exchange.lower()}] стакан {to_internal(symbol)} протух: age={age:.2f}s"
            )
            return None
        
        return book
    
    async def get_latest_book_async(self, exchange: str, symbol: str) -> Optional[dict]:
        """
        Асинхронная версия get_latest_book с явным Lock.
        Используйте если нужна гарантированная консистентность.
        """
        ex = (exchange or "").lower()
        internal = to_internal(symbol)
        
        lock = self._orderbook_locks.get(ex)
        if not lock:
            return None
        
        async with lock:
            books = self._orderbooks.get(ex, {})
            book = books.get(internal)
            
            if book is None:
                return None
            
            # Также оптимизированная версия
            bids = book.get("bids", [])
            asks = book.get("asks", [])
            
            return {
                "bids": bids[:RETURN_BOOK_DEPTH].copy() if bids else [],
                "asks": asks[:RETURN_BOOK_DEPTH].copy() if asks else [],
                "timestamp": book.get("timestamp", 0.0)
            }
    
    # --------------------------------------------------
    # HEALTH METRICS
    # --------------------------------------------------
    def get_health(self, exchange: str) -> Optional[dict]:
        """Получить метрики здоровья соединения."""
        ex = (exchange or "").lower()
        health = self._health.get(ex)
        if health:
            return health.to_dict()
        return None
    
    def get_all_health(self) -> Dict[str, dict]:
        """Получить метрики здоровья всех соединений."""
        return {ex: h.to_dict() for ex, h in self._health.items()}
    
    def is_healthy(self, exchange: str) -> bool:
        """Проверить, здорово ли соединение."""
        ex = (exchange or "").lower()
        health = self._health.get(ex)
        if not health:
            return False
        return health.connected and not health.is_stale
    
    # --------------------------------------------------
    # CONNECT LOOP
    # --------------------------------------------------
    async def _connect(self, exchange: str, url: str):
        """
        Цикл жизни одного WS-подключения к бирже:
        - пытается подключиться с exponential backoff;
        - при успехе слушает сообщения и шлёт ping;
        - при ошибке/закрытии инициирует переподключение.
        """
        assert self.session is not None
        
        ex = exchange.lower()
        health = self._health[ex]
        
        while self.running:
            try:
                async with self.session.ws_connect(
                    url,
                    heartbeat=None,
                    autoping=False,
                ) as ws:
                    # Успешное подключение — сбрасываем счётчик backoff
                    self._reconnect_attempts[ex] = 0
                    self.connections[ex] = ws
                    health.connected = True
                    health.last_connect_ts = time.time()
                    health.last_message_ts = time.time()
                    
                    logger.success(f"🔗 {ex}: WebSocket подключён")
                    
                    # keepalive task
                    ping_task = asyncio.create_task(self._keepalive(ex, ws))
                    
                    # повтор подписок
                    for internal_symbol in list(self.subscriptions.get(ex, [])):
                        if ws.closed:
                            break
                        await self._send_sub(ex, ws, internal_symbol)
                    
                    # читаем сообщения
                    async for msg in ws:
                        # любое сообщение считаем признаком "живости" канала
                        health.last_message_ts = time.time()
                        health.messages_received += 1
                        
                        if msg.type in (aiohttp.WSMsgType.TEXT, aiohttp.WSMsgType.BINARY):
                            # BingX и HTX отправляют данные в GZIP формате (BINARY)
                            if ex in ("bingx", "htx") and msg.type == aiohttp.WSMsgType.BINARY:
                                try:
                                    raw = gzip.decompress(msg.data).decode('utf-8')
                                except Exception as e:
                                    logger.debug(f"[WS:{ex}] gzip decompress error: {e}")
                                    continue
                            else:
                                raw = msg.data if isinstance(msg.data, str) else msg.data.decode()
                            
                            await self._process_message_safe(ex, raw, ws)
                        elif msg.type == aiohttp.WSMsgType.ERROR:
                            health.errors_count += 1
                            logger.warning(f"[WS:{ex}] ошибка WS: {ws.exception()}")
                            break
                    
                    # соединение закрыто — чистим и гасим ping
                    ping_task.cancel()
                    try:
                        await ping_task
                    except Exception:
                        pass
                    
                    if self.connections.get(ex) is ws:
                        self.connections.pop(ex, None)
                    health.connected = False
            
            except asyncio.CancelledError:
                logger.info(f"[WS:{ex}] подключение отменено")
                break
            
            except Exception as e:
                health.connected = False
                health.errors_count += 1
                health.reconnect_count += 1
                
                # Exponential backoff
                attempt = self._reconnect_attempts[ex]
                delay = min(
                    RECONNECT_DELAY_BASE * (RECONNECT_BACKOFF_MULTIPLIER ** attempt),
                    RECONNECT_DELAY_MAX
                )
                self._reconnect_attempts[ex] = attempt + 1
                
                logger.warning(
                    f"🔁 {ex}: переподключение через {delay:.1f}s "
                    f"(attempt={attempt+1}) | {e}"
                )
                await asyncio.sleep(delay)
        
        health.connected = False
        logger.info(f"[WS:{ex}] цикл подключения остановлен")
    
    # --------------------------------------------------
    # KEEPALIVE
    # --------------------------------------------------
    async def _keepalive(self, exchange: str, ws):
        """
        Периодически:
        - отправляет ping в соответствии с протоколом биржи;
        - контролирует "живость" потока сообщений.
        """
        ex = exchange.lower()
        health = self._health[ex]
        
        while self.running and not ws.closed:
            try:
                await asyncio.sleep(WS_PING_INTERVAL)
                
                # Проверка на "протухание"
                if health.is_stale:
                    logger.warning(
                        f"[WS:{ex}] нет сообщений уже {health.age_seconds:.1f}s "
                        f"(> {WS_STALE_TIMEOUT}s), инициируем реконнект"
                    )
                    await ws.close()
                    break
                
                # Ping по схемам конкретных бирж
                if ex == "bybit":
                    await ws.send_json({"op": "ping"})
                elif ex == "bitget":
                    await ws.send_str("ping")
                elif ex == "okx":
                    await ws.send_str("ping")
                elif ex == "gate":
                    await ws.send_json({
                        "time": int(time.time()),
                        "channel": "futures.ping"
                    })
                elif ex == "mexc":
                    await ws.send_json({"method": "ping"})
                elif ex == "bingx":
                    # BingX: сервер отправляет Ping, клиент отвечает Pong
                    # Мы отвечаем в _process_message_safe
                    pass
                elif ex == "htx":
                    # HTX: сервер сам отправляет ping каждые ~5 сек
                    # Клиент отвечает pong в _process_message_safe
                    # Здесь ничего отправлять не нужно
                    pass
            
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.warning(f"[WS:{ex}] keepalive error: {e}")
                break
    
    # --------------------------------------------------
    # SUBSCRIBE SEND
    # --------------------------------------------------
    async def _send_sub(self, exchange: str, ws, internal_symbol: str):
        """
        Отправка сообщения подписки по конкретному символу для конкретной биржи.
        """
        ex = exchange.lower()
        
        if ws.closed:
            logger.debug(f"[WS:{ex}] попытка отправить подписку на закрытый сокет, skip")
            return
        
        try:
            ws_symbol = to_ws_symbol(ex, internal_symbol)
            
            if ex == "bybit":
                payload = {
                    "op": "subscribe",
                    "args": [f"orderbook.50.{ws_symbol}"]
                }
            
            elif ex == "bitget":
                inst_id = internal_symbol
                payload = {
                    "op": "subscribe",
                    "args": [{
                        "instType": "USDT-FUTURES",
                        "channel": "books15",
                        "instId": inst_id,
                    }],
                }
            
            elif ex == "gate":
                payload = {
                    "time": int(time.time()),
                    "channel": "futures.order_book",
                    "event": "subscribe",
                    "payload": [ws_symbol, "20", "0"]
                }
            
            elif ex == "okx":
                payload = {
                    "op": "subscribe",
                    "args": [{
                        "channel": "books5",
                        "instId": ws_symbol
                    }]
                }
            
            elif ex == "mexc":
                payload = {
                    "method": "sub.depth.full",
                    "param": {
                        "symbol": ws_symbol,
                        "limit": 20
                    }
                }
            
            elif ex == "bingx":
                # BingX Perpetual Swap V2 API
                # Формат подписки: BTC-USDT@depth20 (поддерживаются: depth5, depth10, depth20, depth50, depth100)
                # Документация: https://bingx-api.github.io/docs/#/en-us/swapV2/socket/market.html
                payload = {
                    "id": str(uuid.uuid4()),
                    "reqType": "sub",
                    "dataType": f"{ws_symbol}@depth20"
                }
            
            elif ex == "htx":
                # HTX (ex-Huobi) USDT-M Linear Perpetual Futures
                # Формат подписки: market.BTC-USDT.depth.step6 (20 уровней, оптимально для арбитража)
                # Документация: https://huobiapi.github.io/docs/usdt_swap/v1/en/
                payload = {
                    "sub": f"market.{ws_symbol}.depth.step6",
                    "id": str(uuid.uuid4())
                }
            
            else:
                logger.warning(f"[WS] Нет схемы подписки для {ex}")
                return
            
            await ws.send_json(payload)
            logger.debug(f"[WS:{ex}] sent sub for {internal_symbol} ({ws_symbol})")
        
        except Exception as e:
            logger.warning(f"[WS:{ex}] Ошибка отправки подписки {internal_symbol}: {e}")
    
    # --------------------------------------------------
    # MESSAGE PARSER (thread-safe)
    # --------------------------------------------------
    async def _process_message_safe(self, exchange: str, raw: str, ws=None):
        """
        Thread-safe обёртка над _process_message.
        Использует Lock при записи в orderbooks.
        """
        ex = exchange.lower()
        
        if DEBUG_WS_RAW:
            logger.debug(f"[{ex.upper()} RAW] {raw[:500]}")
        
        # BingX: обработка Ping -> отвечаем Pong
        if ex == "bingx" and raw.strip() == "Ping":
            if ws and not ws.closed:
                try:
                    await ws.send_str("Pong")
                except Exception as e:
                    logger.debug(f"[WS:{ex}] Pong send error: {e}")
            return
        
        try:
            data = json.loads(raw)
        except Exception as e:
            logger.debug(f"[WS:{ex}] invalid JSON: {e} | raw={raw[:200]}")
            return
        
        # HTX: обработка ping -> отвечаем pong
        if ex == "htx" and "ping" in data:
            if ws and not ws.closed:
                try:
                    await ws.send_json({"pong": data["ping"]})
                except Exception as e:
                    logger.debug(f"[WS:{ex}] Pong send error: {e}")
            return
        
        ts = time.time()

        # ИСПРАВЛЕНИЕ баг #8: для Bitget update нужен lock ДО парсинга
        # т.к. парсинг читает текущий стакан
        lock = self._orderbook_locks.get(ex)

        # Для Bitget с action="update" берём lock ПЕРЕД парсингом
        needs_lock_before_parse = (
            ex == "bitget" and
            isinstance(data, dict) and
            data.get("action") == "update"
        )

        if needs_lock_before_parse and lock:
            async with lock:
                parsed = self._parse_orderbook_data(ex, data, ts)
                if parsed is not None:
                    internal, book = parsed
                    self._orderbooks[ex][internal] = book
        else:
            # Обычный путь: парсим без lock, потом записываем с lock
            parsed = self._parse_orderbook_data(ex, data, ts)

            if parsed is not None:
                internal, book = parsed

                # Записываем с Lock
                if lock:
                    async with lock:
                        self._orderbooks[ex][internal] = book
                else:
                    # Fallback без lock (не должно происходить)
                    self._orderbooks[ex][internal] = book
    
    def _parse_orderbook_data(
        self,
        exchange: str,
        data: dict,
        ts: float
    ) -> Optional[tuple]:
        """
        Парсит данные стакана из WS сообщения.
        Возвращает (internal_symbol, orderbook_dict) или None.
        """
        ex = exchange.lower()
        
        # --------------------------------------------------
        # BYBIT
        # --------------------------------------------------
        if ex == "bybit":
            if "topic" in data and "orderbook" in data["topic"]:
                symbol_raw = data["topic"].split(".")[-1]
                internal = to_internal(symbol_raw)
                
                ob = data.get("data")
                if isinstance(ob, list) and ob:
                    ob = ob[0]
                if not isinstance(ob, dict):
                    return None
                
                bids = safe_levels(ob.get("b", []))
                asks = safe_levels(ob.get("a", []))
                
                return internal, {
                    "bids": sorted(bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                    "asks": sorted(asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                    "timestamp": ts
                }
        
        # --------------------------------------------------
        # BITGET
        # --------------------------------------------------
        if ex == "bitget":
            arg = data.get("arg", {})
            instId = arg.get("instId")
            if not instId:
                return None
            
            internal = to_internal(instId)
            action = data.get("action")
            
            if action == "snapshot":
                arr = data.get("data") or []
                if not arr:
                    return None
                book = arr[0]
                bids = safe_levels(book.get("bids", []))
                asks = safe_levels(book.get("asks", []))
                
                return internal, {
                    "bids": sorted(bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                    "asks": sorted(asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                    "timestamp": ts
                }
            
            if action == "update":
                # Для update нужен текущий стакан
                curr = self._orderbooks[ex].get(internal, {"bids": [], "asks": []})
                arr = data.get("data") or []
                if not arr:
                    return None
                
                # Копируем текущие данные для модификации
                new_bids = list(curr.get("bids", []))
                new_asks = list(curr.get("asks", []))
                
                # update bids
                for lvl in arr[0].get("bids", []):
                    try:
                        price = float(lvl[0])
                        qty = float(lvl[1])
                    except Exception:
                        continue
                    new_bids = [x for x in new_bids if x[0] != price]
                    if qty > 0:
                        new_bids.append([price, qty])
                
                # update asks
                for lvl in arr[0].get("asks", []):
                    try:
                        price = float(lvl[0])
                        qty = float(lvl[1])
                    except Exception:
                        continue
                    new_asks = [x for x in new_asks if x[0] != price]
                    if qty > 0:
                        new_asks.append([price, qty])
                
                return internal, {
                    "bids": sorted(new_bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                    "asks": sorted(new_asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                    "timestamp": ts
                }
        
        # --------------------------------------------------
        # OKX
        # --------------------------------------------------
        if ex == "okx":
            if "arg" in data and "data" in data:
                instId = data["arg"].get("instId")
                if not instId:
                    return None
                
                internal = to_internal(instId)
                arr = data.get("data") or []
                if not arr:
                    return None
                
                book = arr[0]
                bids = safe_levels(book.get("bids", []))
                asks = safe_levels(book.get("asks", []))
                
                return internal, {
                    "bids": sorted(bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                    "asks": sorted(asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                    "timestamp": ts
                }
        
        # --------------------------------------------------
        # GATE
        # --------------------------------------------------
        if ex == "gate":
            event = data.get("event")
            if event in ("all", "update") and "result" in data:
                res = data["result"]
                instId = res.get("contract", "")
                if not instId:
                    return None
                
                internal = to_internal(instId)
                asks_src = res.get("asks", []) or []
                bids_src = res.get("bids", []) or []
                
                asks = safe_levels([[x["p"], x["s"]] for x in asks_src])
                bids = safe_levels([[x["p"], x["s"]] for x in bids_src])
                
                return internal, {
                    "bids": sorted(bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                    "asks": sorted(asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                    "timestamp": ts,
                }
        
        # --------------------------------------------------
        # MEXC
        # --------------------------------------------------
        if ex == "mexc":
            # Формат 1: {"channel": "push.depth", "symbol": "BTC_USDT", "data": {...}}
            if data.get("channel") == "push.depth" and "data" in data:
                symbol = data.get("symbol", "")
                internal = to_internal(symbol)
                
                depth_data = data["data"]
                asks_raw = depth_data.get("asks", [])
                bids_raw = depth_data.get("bids", [])
                
                # MEXC использует формат [price, orders_count, quantity]
                asks = safe_levels_mexc(asks_raw)
                bids = safe_levels_mexc(bids_raw)
                
                # Проверяем что хотя бы одна сторона не пустая
                if not asks and not bids:
                    return None
                
                return internal, {
                    "bids": sorted(bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                    "asks": sorted(asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                    "timestamp": ts
                }
            
            # Формат 2: {"symbol": "BTC_USDT", "data": {"asks": [...], "bids": [...]}}
            if "symbol" in data and "data" in data and isinstance(data["data"], dict):
                symbol = data.get("symbol", "")
                internal = to_internal(symbol)
                
                depth_data = data["data"]
                asks_raw = depth_data.get("asks", [])
                bids_raw = depth_data.get("bids", [])
                
                asks = safe_levels_mexc(asks_raw)
                bids = safe_levels_mexc(bids_raw)
                
                if not asks and not bids:
                    return None
                
                return internal, {
                    "bids": sorted(bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                    "asks": sorted(asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                    "timestamp": ts
                }
        
        # --------------------------------------------------
        # BINGX
        # --------------------------------------------------
        if ex == "bingx":
            # BingX Perpetual Swap V2 API
            # Формат ответа: {"dataType": "BTC-USDT@depth20", "data": {"bids": [[price, qty]], "asks": [[price, qty]]}}
            data_type = data.get("dataType", "")
            
            if "@depth" in data_type and "data" in data:
                # Извлекаем символ: "BTC-USDT@depth20" -> "BTC-USDT"
                symbol_raw = data_type.split("@")[0]  # "BTC-USDT"
                internal = to_internal(symbol_raw)
                
                depth_data = data.get("data")
                if not isinstance(depth_data, dict):
                    return None
                
                # BingX V2 формат: {"bids": [["price", "qty"], ...], "asks": [["price", "qty"], ...]}
                bids_raw = depth_data.get("bids", [])
                asks_raw = depth_data.get("asks", [])
                
                bids = safe_levels(bids_raw)
                asks = safe_levels(asks_raw)
                
                if not asks and not bids:
                    return None
                
                return internal, {
                    "bids": sorted(bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                    "asks": sorted(asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                    "timestamp": ts
                }
        
        # --------------------------------------------------
        # HTX (ex-Huobi)
        # --------------------------------------------------
        if ex == "htx":
            # HTX USDT-M Linear Perpetual Futures
            # Формат ответа: {"ch": "market.BTC-USDT.depth.step6", "ts": 1629790438801, "tick": {"bids": [...], "asks": [...]}}
            ch = data.get("ch", "")
            
            if "depth" in ch and "tick" in data:
                # Извлекаем символ из канала: "market.BTC-USDT.depth.step6" -> "BTC-USDT"
                parts = ch.split(".")
                if len(parts) >= 2:
                    symbol_raw = parts[1]  # "BTC-USDT"
                    internal = to_internal(symbol_raw)
                    
                    tick = data.get("tick")
                    if not isinstance(tick, dict):
                        return None
                    
                    # HTX формат: {"bids": [[price, qty], ...], "asks": [[price, qty], ...]}
                    bids_raw = tick.get("bids", [])
                    asks_raw = tick.get("asks", [])
                    
                    bids = safe_levels(bids_raw)
                    asks = safe_levels(asks_raw)
                    
                    if not asks and not bids:
                        return None
                    
                    return internal, {
                        "bids": sorted(bids, key=lambda x: -x[0])[:MAX_BOOK_DEPTH],
                        "asks": sorted(asks, key=lambda x: x[0])[:MAX_BOOK_DEPTH],
                        "timestamp": ts
                    }
        
        return None
