package prices

import (
	"fmt"
	"sync"
	"time"

	"arbitrage-terminal/internal/exchanges"
)

// PriceUpdateCallback вызывается при каждом обновлении цены.
// Используется для отправки события в Coordinator для обработки.
type PriceUpdateCallback func(update *exchanges.PriceUpdate)

// Aggregator собирает цены со всех бирж и распределяет их по трекерам.
// Один трекер на символ. Потокобезопасен.
type Aggregator struct {
	trackers    map[string]*Tracker          // symbol -> Tracker
	exchanges   map[string]exchanges.Exchange // exchange name -> Exchange client
	callback    PriceUpdateCallback          // Callback для отправки событий в Coordinator
	mu          sync.RWMutex                 // Защита trackers
	closeCh     chan struct{}                // Канал для graceful shutdown
}

// NewAggregator создаёт новый Aggregator.
//
// Параметры:
//   - exchanges: карта коннекторов бирж (name -> Exchange)
//   - callback: функция-обработчик обновлений цен (отправка в Coordinator)
func NewAggregator(
	exchanges map[string]exchanges.Exchange,
	callback PriceUpdateCallback,
) *Aggregator {
	return &Aggregator{
		trackers:  make(map[string]*Tracker),
		exchanges: exchanges,
		callback:  callback,
		closeCh:   make(chan struct{}),
	}
}

// Subscribe подписывается на указанный символ на всех биржах.
// Создаёт трекер для символа и подписывается на WebSocket обновления.
func (a *Aggregator) Subscribe(symbol string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Проверить: может трекер уже существует
	if _, exists := a.trackers[symbol]; exists {
		return fmt.Errorf("already subscribed to symbol %s", symbol)
	}

	// Создать трекер для символа
	tracker := NewTracker(symbol)
	a.trackers[symbol] = tracker

	// Подписаться на каждой бирже через WebSocket
	// ПРИМЕЧАНИЕ: В реальности подписка происходит через WebSocket клиент,
	// который вызовет handlePriceUpdate() при получении данных.
	// Здесь только инициализация.

	return nil
}

// Unsubscribe отписывается от указанного символа.
func (a *Aggregator) Unsubscribe(symbol string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	tracker, exists := a.trackers[symbol]
	if !exists {
		return fmt.Errorf("not subscribed to symbol %s", symbol)
	}

	// Очистить данные трекера
	tracker.Clear()

	// Удалить трекер
	delete(a.trackers, symbol)

	// ПРИМЕЧАНИЕ: В реальности здесь нужно отписаться от WebSocket
	// каждой биржи для этого символа.

	return nil
}

// HandlePriceUpdate обрабатывает обновление цены от биржи.
// Этот метод вызывается WebSocket клиентом каждой биржи при получении данных.
//
// Параметры:
//   - update: обновление цены от биржи
//
// Логика:
//  1. Обновить трекер для символа
//  2. Вызвать callback для отправки события в Coordinator
func (a *Aggregator) HandlePriceUpdate(update *exchanges.PriceUpdate) {
	// Защита от nil - может возникнуть при ошибках WebSocket или очереди
	if update == nil {
		return
	}

	a.mu.RLock()
	tracker, exists := a.trackers[update.Symbol]
	a.mu.RUnlock()

	if !exists {
		// Символ не подписан - игнорировать
		return
	}

	// Обновить данные трекера
	tracker.Update(update.Exchange, &ExchangePrice{
		BestBid:   update.BestBid,
		BestAsk:   update.BestAsk,
		Orderbook: update.Orderbook,
		UpdatedAt: update.Timestamp,
	})

	// Вызвать callback для отправки события в Coordinator
	if a.callback != nil {
		a.callback(update)
	}
}

// GetTracker возвращает трекер для указанного символа.
// Используется в Pair для получения лучших цен.
func (a *Aggregator) GetTracker(symbol string) (*Tracker, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	tracker, exists := a.trackers[symbol]
	if !exists {
		return nil, fmt.Errorf("no tracker for symbol %s", symbol)
	}

	return tracker, nil
}

// GetAllSymbols возвращает список всех отслеживаемых символов.
func (a *Aggregator) GetAllSymbols() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	symbols := make([]string, 0, len(a.trackers))
	for symbol := range a.trackers {
		symbols = append(symbols, symbol)
	}

	return symbols
}

// Start запускает Aggregator.
// Подключает все биржи через WebSocket и начинает получать обновления.
func (a *Aggregator) Start() error {
	// Подключиться ко всем биржам
	for name, exch := range a.exchanges {
		if err := exch.Connect(); err != nil {
			return fmt.Errorf("failed to connect to exchange %s: %w", name, err)
		}
	}

	// Запустить мониторинг устаревших данных
	go a.monitorStaleData()

	return nil
}

// Stop останавливает Aggregator и закрывает все соединения.
func (a *Aggregator) Stop() error {
	close(a.closeCh)

	// Отключиться от всех бирж
	for name, exch := range a.exchanges {
		if err := exch.Close(); err != nil {
			return fmt.Errorf("failed to close exchange %s: %w", name, err)
		}
	}

	return nil
}

// monitorStaleData отслеживает устаревшие данные и логирует предупреждения.
// Данные считаются устаревшими, если не обновлялись более 5 секунд.
func (a *Aggregator) monitorStaleData() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.checkStaleData()

		case <-a.closeCh:
			return
		}
	}
}

// checkStaleData проверяет все трекеры на наличие устаревших данных.
func (a *Aggregator) checkStaleData() {
	maxAge := 5 * time.Second

	a.mu.RLock()
	trackers := make(map[string]*Tracker, len(a.trackers))
	for symbol, tracker := range a.trackers {
		trackers[symbol] = tracker
	}
	a.mu.RUnlock()

	for _, tracker := range trackers {
		for _, exchName := range tracker.GetExchanges() {
			if tracker.IsStale(exchName, maxAge) {
				// ПРИМЕЧАНИЕ: Здесь должно быть логирование
				// log.Warn("Stale data detected", "symbol", tracker.GetSymbol(), "exchange", exchName)

				// TODO: Можно попытаться переподключиться к WebSocket
			}
		}
	}
}

// GetBestPricesForSymbol возвращает лучшие цены для указанного символа.
// Удобный метод для быстрого доступа.
func (a *Aggregator) GetBestPricesForSymbol(symbol string) (*BestPrices, error) {
	tracker, err := a.GetTracker(symbol)
	if err != nil {
		return nil, err
	}

	bestPrices := tracker.GetBestPrices()
	if bestPrices == nil {
		return nil, fmt.Errorf("no price data for symbol %s", symbol)
	}

	return bestPrices, nil
}
