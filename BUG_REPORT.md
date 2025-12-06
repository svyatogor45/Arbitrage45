# 🐛 Отчёт об анализе багов в Arbitrage45

**Дата анализа:** 2025-12-06
**Анализатор:** Claude (Sonnet 4.5)
**Всего найдено багов:** 13

---

## 🔴 КРИТИЧНЫЕ БАГИ (приводят к падению программы)

### 1. Отсутствуют константы CRITICAL_IMBALANCE_PCT и WARNING_IMBALANCE_PCT

**Файл:** `config.py`
**Связанный файл:** `trade_engine.py:26`

**Проблема:**
```python
# trade_engine.py строка 26
from config import CRITICAL_IMBALANCE_PCT, WARNING_IMBALANCE_PCT
# ❌ ImportError: cannot import name 'CRITICAL_IMBALANCE_PCT'
```

**Последствия:**
- Бот не запустится
- ImportError при старте

**Решение:**
Добавить в `config.py`:
```python
# Пороги дисбаланса объёмов между ногами
WARNING_IMBALANCE_PCT = 5.0    # 5% - предупреждение
CRITICAL_IMBALANCE_PCT = 10.0  # 10% - критический дисбаланс
```

---

### 2. Отсутствует константа PRICE_UPDATE_INTERVAL

**Файл:** `config.py`
**Связанный файл:** `main.py:21`

**Проблема:**
```python
# main.py строка 461
await asyncio.sleep(PRICE_UPDATE_INTERVAL)
# ❌ NameError: name 'PRICE_UPDATE_INTERVAL' is not defined
```

**Последствия:**
- Бот не запустится
- NameError в главном цикле

**Решение:**
Добавить в `config.py`:
```python
# Интервал обновления цен (секунды)
PRICE_UPDATE_INTERVAL = 1.0  # 1 секунда между тиками
```

---

### 3. Деление на ноль при расчёте PnL

**Файл:** `main.py:969-970`

**Проблема:**
```python
# Если entry_prices_long или entry_prices_short пустые
avg_long_entry = sum(state.entry_prices_long) / len(state.entry_prices_long)
avg_short_entry = sum(state.entry_prices_short) / len(state.entry_prices_short)
# ❌ ZeroDivisionError!
```

**Как проявляется:**
1. Позиция восстановлена из БД с пустыми массивами цен
2. Бот пытается рассчитать PnL
3. ZeroDivisionError → бот падает

**Пример:**
```
Восстановленная позиция:
- filled_parts = 1
- entry_prices_long = []  # пустой!
- entry_prices_short = [40000.0]

Расчёт PnL:
>>> sum([]) / len([])
ZeroDivisionError: division by zero
```

**Решение:**
```python
if not state.entry_prices_long or not state.entry_prices_short:
    logger.error(f"[{state.pair_id}] Пустые массивы цен входа!")
    return

avg_long_entry = sum(state.entry_prices_long) / len(state.entry_prices_long)
avg_short_entry = sum(state.entry_prices_short) / len(state.entry_prices_short)
```

---

### 4. Race condition в RiskController.refresh_from_state

**Файл:** `main.py:163-176`

**Проблема:**
```python
async def refresh_from_state(self, pair_states: Dict[int, "PairState"]):
    async with self._lock:
        self._open_pairs_count = sum(
            1 for s in pair_states.values()  # ❌ pair_states может изменяться!
            if s.open_parts > 0
        )
```

**Как проявляется:**
При параллельной обработке пар:
1. Корутина A начинает итерацию `pair_states.values()`
2. Корутина B добавляет/удаляет пару из `pair_states`
3. RuntimeError: dictionary changed size during iteration

**Пример лога:**
```
RuntimeError: dictionary changed size during iteration
  at main.py:169 in refresh_from_state
    for s in pair_states.values()
```

**Решение:**
```python
async def refresh_from_state(self, pair_states: Dict[int, "PairState"]):
    async with self._lock:
        # Создаём snapshot для безопасной итерации
        snapshot = dict(pair_states)
        self._open_pairs_count = sum(
            1 for s in snapshot.values()
            if s.open_parts > 0
        )
```

---

### 5. Утечка памяти в кэше WebSocket стаканов

**Файл:** `ws_manager.py`

**Проблема:**
- Стаканы добавляются в `_orderbooks` при подписке
- Но НИКОГДА не удаляются, даже когда символ больше не нужен

**Как проявляется:**
```
День 1: торговали 30 пар → 30 стаканов в памяти (50 MB)
День 2: поменяли на другие 30 пар → 60 стаканов в памяти (100 MB)
...
Неделя 1: протестировали 200 пар → 200 стаканов в памяти (800 MB)
Месяц 1: 1000+ стаканов → 4+ GB → Out of Memory!
```

**Последствия:**
- Постоянный рост использования памяти
- Через несколько недель бот упадет от нехватки памяти
- Замедление работы из-за большого словаря

**Решение:**
Добавить метод автоочистки:
```python
async def _cleanup_unused_orderbooks(self):
    """Удалить стаканы для неиспользуемых символов."""
    while self.running:
        await asyncio.sleep(3600)  # каждый час

        for ex in list(self._orderbooks.keys()):
            subscribed = self.subscriptions.get(ex, set())
            cached = set(self._orderbooks[ex].keys())

            # Удаляем стаканы, на которые нет подписки
            unused = cached - subscribed
            for symbol in unused:
                async with self._orderbook_locks[ex]:
                    self._orderbooks[ex].pop(symbol, None)

            if unused:
                logger.info(f"🧹 [{ex}] Очищено {len(unused)} неиспользуемых стаканов")
```

---

### 6. Кэш спредов в MarketEngine не очищается

**Файл:** `market_engine.py`

**Проблема:**
```python
def cleanup_stale_cache(self):
    """Удалить устаревшие записи из кэша."""
    # ✅ Метод есть
    # ❌ Но кто его вызовет? НИКТО!
```

**Как проявляется:**
- Кэш `_spread_cache` растет бесконечно
- Для 7 бирж × 30 символов = ~1470 записей в кэше
- Каждая запись живет вечно
- TTL = 0.9 сек, но очистка не происходит

**Решение:**
```python
# В main.py добавить периодическую очистку:
async def periodic_cache_cleanup(market_engine):
    while True:
        await asyncio.sleep(60)  # каждую минуту
        market_engine.cleanup_stale_cache()

# В main():
asyncio.create_task(periodic_cache_cleanup(market))
```

---

## 🟡 СРЕДНИЕ БАГИ (логические ошибки)

### 7. Дисбаланс объемов не сбрасывается при выходе

**Файл:** `main.py:142-144`

**Проблема:**
```python
def reset_after_exit(self):
    self.filled_parts = 0
    self.closed_parts = 0
    # ...
    # ❌ Забыли:
    # self.actual_long_volume = 0.0
    # self.actual_short_volume = 0.0
```

**Как проявляется:**
```
Цикл 1:
  Вход 1.0 BTC → actual_long_volume=1.0, actual_short_volume=0.99
  Выход → НЕ сброшено!

Цикл 2:
  Вход 1.0 BTC → actual_long_volume=2.0 ❌ (должно быть 1.0!)
  Дисбаланс считается неправильно!
```

**Решение:**
```python
def reset_after_exit(self):
    # ... существующий код ...
    self.actual_long_volume = 0.0
    self.actual_short_volume = 0.0
```

---

### 8. Race condition при обновлении стакана Bitget

**Файл:** `ws_manager.py:709-744`

**Проблема:**
```python
if action == "update":
    # ❌ Читаем БЕЗ lock
    curr = self._orderbooks[ex].get(internal, {"bids": [], "asks": []})

    # ... модифицируем ...

    # ✅ Пишем С lock
    async with lock:
        self._orderbooks[ex][internal] = book
```

**Как проявляется:**
При высокой частоте обновлений от Bitget:
1. Update #1 читает стакан
2. Update #2 читает тот же стакан
3. Update #1 применяет изменения и пишет
4. Update #2 применяет изменения и пишет → перезаписывает #1!
5. Потеряли часть обновлений → неточные цены

**Решение:**
```python
if action == "update":
    async with lock:  # ✅ Взять lock ПЕРЕД чтением
        curr = self._orderbooks[ex].get(internal, {"bids": [], "asks": []})
        # ... модифицируем ...
        self._orderbooks[ex][internal] = book
```

---

### 9. Не корректируются объемы при частичном выходе

**Файл:** `main.py:1117`

**Проблема:**
При частичном закрытии позиции не уменьшаются `actual_long_volume` и `actual_short_volume`.

**Как проявляется:**
```
Открыли 3 части по 1 BTC:
  actual_long_volume = 3.0
  actual_short_volume = 2.97

Закрыли 1 часть:
  actual_long_volume = 3.0 ❌ (должно 2.0!)
  actual_short_volume = 2.97 ❌ (должно 1.98!)
```

**Решение:**
```python
if res["success"]:
    state.closed_parts += 1

    # Уменьшаем фактические объёмы
    if state.actual_long_volume > 0:
        state.actual_long_volume -= volume_to_close
    if state.actual_short_volume > 0:
        state.actual_short_volume -= volume_to_close
```

---

### 10. ConnectionHealth не thread-safe

**Файл:** `exchange_manager.py:83-94`

**Проблема:**
```python
def record_request(self, success: bool, error_msg: str = ""):
    self.requests_total += 1  # ❌ race condition!
    # При параллельных запросах счётчик может быть неправильным
```

**Как проявляется:**
При 1000 запросов в секунду:
- Ожидаемо: `requests_total = 1000`
- Реально: `requests_total = 987` (потеряли 13 инкрементов)

**Решение:**
Использовать threading.Lock или атомарные операции.

---

### 11. Некорректная обработка None в save_position

**Файл:** `db_manager.py:472-508`

**Проблема:**
```python
def save_position(
    ...,
    actual_long_volume: Optional[float] = None,
    actual_short_volume: Optional[float] = None,
):
    return self._execute(
        """...""",
        (..., actual_long_volume, actual_short_volume)  # ❌ None → NULL в БД
    )
```

**Последствия:**
В БД запишется NULL, что приведет к несогласованности.

**Решение:**
```python
actual_long_volume = actual_long_volume if actual_long_volume is not None else 0.0
actual_short_volume = actual_short_volume if actual_short_volume is not None else 0.0
```

---

### 12. Нет логирования фактических объемов при выходе

**Файл:** `trade_engine.py:899-1064`

**Проблема:**
При закрытии позиции не возвращается информация о фактически исполненных объёмах.

**Решение:**
```python
return {
    "success": True,
    "exit_long_order": long_order,
    "exit_short_order": short_order,
    "filled_long": long_order.get("filled") or 0.0,
    "filled_short": short_order.get("filled") or 0.0,
    "error": None,
}
```

---

### 13. Отсутствует проверка свежести в get_latest_book

**Файл:** `ws_manager.py:243-273`

**Проблема:**
Метод возвращает стакан без проверки возраста. Может вернуть 5-минутной давности данные.

**Решение:**
Добавить опциональную проверку возраста или использовать только `get_fresh_book`.

---

## 📊 ИТОГОВАЯ СТАТИСТИКА

| Категория | Количество |
|-----------|------------|
| **Критичные** (падение программы) | 6 |
| **Средние** (логические ошибки) | 7 |
| **ВСЕГО** | **13** |

## 🎯 ПРИОРИТЕТЫ ИСПРАВЛЕНИЯ

### Срочно (без этого бот не работает):
1. ✅ Добавить `CRITICAL_IMBALANCE_PCT` и `WARNING_IMBALANCE_PCT` в config.py
2. ✅ Добавить `PRICE_UPDATE_INTERVAL` в config.py
3. ✅ Защита от деления на ноль в main.py:969-970

### Важно (проблемы при длительной работе):
4. ✅ Race condition в RiskController
5. ✅ Утечка памяти в кэше WebSocket
6. ✅ Автоочистка кэша спредов

### Желательно (улучшение надежности):
7. ✅ Сброс actual_volume при выходе
8. ✅ Race condition в Bitget updates
9. ✅ Коррекция объемов при частичном выходе
10. ✅ Thread-safety для ConnectionHealth

---

**Конец отчёта**
