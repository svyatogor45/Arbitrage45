package prices

import (
	"fmt"
	"sync"
	"time"

	"arbitrage-terminal/internal/exchanges"
	"go.uber.org/zap"
)

// PriceUpdateCallback вызывается при каждом обновлении цены.
// Используется для отправки события в Coordinator для обработки.
type PriceUpdateCallback func(update *exchanges.PriceUpdate)

// Aggregator собирает цены со всех бирж и распределяет их по трекерам.
// Один трекер на символ. Потокобезопасен.
type Aggregator struct {
	trackers     map[string]*Tracker          // symbol -> Tracker
	exchanges    map[string]exchanges.Exchange // exchange name -> Exchange client
	callback     PriceUpdateCallback          // Callback для отправки событий в Coordinator
	logger       *zap.Logger                  // Логгер для записи событий
	mu           sync.RWMutex                 // Защита trackers
	closeCh      chan struct{}                // Канал для graceful shutdown
	closeOnce    sync.Once                    // Защита от повторного close(closeCh)
	wg           sync.WaitGroup               // Ожидание завершения горутин
}

// NewAggregator создаёт новый Aggregator.
//
// Параметры:
//   - exchs: карта коннекторов бирж (name -> Exchange)
//   - callback: функция-обработчик обновлений цен (отправка в Coordinator)
//   - logger: логгер zap для записи событий
func NewAggregator(
	exchs map[string]exchanges.Exchange,
	callback PriceUpdateCallback,
	logger *zap.Logger,
) *Aggregator {
	// Если логгер не передан, используем no-op логгер
	if logger == nil {
		logger = zap.NewNop()
	}

	return &Aggregator{
		trackers:  make(map[string]*Tracker),
		exchanges: exchs,
		callback:  callback,
		logger:    logger,
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
	a.logger.Info("Запуск Aggregator", zap.Int("exchanges_count", len(a.exchanges)))

	// Подключиться ко всем биржам
	for name, exch := range a.exchanges {
		if err := exch.Connect(); err != nil {
			a.logger.Error("Не удалось подключиться к бирже",
				zap.String("exchange", name),
				zap.Error(err),
			)
			return fmt.Errorf("failed to connect to exchange %s: %w", name, err)
		}
		a.logger.Info("Подключение к бирже установлено", zap.String("exchange", name))
	}

	// Запустить мониторинг устаревших данных
	a.wg.Add(1)
	go a.monitorStaleData()

	a.logger.Info("Aggregator успешно запущен")
	return nil
}

// Stop останавливает Aggregator и закрывает все соединения.
// Безопасен для многократного вызова благодаря sync.Once.
func (a *Aggregator) Stop() error {
	a.logger.Info("Остановка Aggregator...")

	// Защита от повторного закрытия канала (паника)
	a.closeOnce.Do(func() {
		close(a.closeCh)
	})

	// Дождаться завершения всех горутин
	a.wg.Wait()

	// Отключиться от всех бирж
	for name, exch := range a.exchanges {
		if err := exch.Close(); err != nil {
			a.logger.Error("Не удалось отключиться от биржи",
				zap.String("exchange", name),
				zap.Error(err),
			)
			return fmt.Errorf("failed to close exchange %s: %w", name, err)
		}
		a.logger.Info("Отключение от биржи выполнено", zap.String("exchange", name))
	}

	a.logger.Info("Aggregator остановлен")
	return nil
}

// monitorStaleData отслеживает устаревшие данные и логирует предупреждения.
// Данные считаются устаревшими, если не обновлялись более 5 секунд.
func (a *Aggregator) monitorStaleData() {
	defer a.wg.Done() // Сигнализируем о завершении горутины

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
				a.logger.Warn("Обнаружены устаревшие данные",
					zap.String("symbol", tracker.GetSymbol()),
					zap.String("exchange", exchName),
					zap.Duration("max_age", maxAge),
				)

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
