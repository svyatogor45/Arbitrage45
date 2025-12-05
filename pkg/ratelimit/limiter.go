package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Limiter реализует алгоритм Token Bucket для rate limiting.
// Потокобезопасен (thread-safe).
//
// Token Bucket работает следующим образом:
//  1. Есть корзина (bucket) с токенами фиксированной ёмкости
//  2. Токены добавляются с постоянной скоростью (refill rate)
//  3. Каждый запрос забирает 1 токен из корзины
//  4. Если токенов нет - запрос ждёт или отклоняется
//
// Пример:
//
//	// Создать limiter: 100 запросов в минуту, burst = 10
//	limiter := NewLimiter(100, 10, time.Minute)
//
//	// Ждать токен (блокирующий вызов)
//	limiter.Wait(context.Background())
//
//	// Попытаться получить токен без ожидания
//	if limiter.TryAcquire() {
//	    // Токен получен, можно делать запрос
//	}
type Limiter struct {
	tokens       float64       // Текущее количество токенов в корзине
	maxTokens    float64       // Максимальная ёмкость корзины (burst)
	refillRate   float64       // Скорость добавления токенов (в секунду)
	lastRefill   time.Time     // Время последнего пополнения
	mu           sync.Mutex    // Мутекс для защиты tokens и lastRefill
}

// NewLimiter создаёт новый Rate Limiter.
//
// Параметры:
//   - requestsPerPeriod: количество разрешённых запросов за период
//   - burst: максимальное количество запросов в burst режиме
//   - period: период времени (например, time.Minute для "запросов в минуту")
//
// Пример:
//
//	// 120 запросов в минуту, burst = 20
//	limiter := NewLimiter(120, 20, time.Minute)
func NewLimiter(requestsPerPeriod int, burst int, period time.Duration) *Limiter {
	// Рассчитать скорость пополнения токенов (в секунду)
	refillRate := float64(requestsPerPeriod) / period.Seconds()

	return &Limiter{
		tokens:     float64(burst), // Начинаем с полной корзины
		maxTokens:  float64(burst),
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// Wait ждёт, пока не станет доступен токен, затем забирает его.
// Блокирующий вызов. Возвращает ошибку, если контекст отменён.
//
// Параметры:
//   - ctx: контекст для отмены ожидания
//
// Возвращает ошибку, если контекст отменён до получения токена.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		// Попытаться получить токен
		if l.TryAcquire() {
			return nil
		}

		// Проверить, не отменён ли контекст
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Подождать немного перед следующей попыткой
		// Время ожидания = время до следующего токена
		l.mu.Lock()
		waitTime := l.timeUntilNextToken()
		l.mu.Unlock()

		// Спать минимум 1ms, максимум waitTime
		sleepTime := minDuration(waitTime, 100*time.Millisecond)
		if sleepTime < 1*time.Millisecond {
			sleepTime = 1 * time.Millisecond
		}

		timer := time.NewTimer(sleepTime)
		select {
		case <-timer.C:
			// Продолжить попытки
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// TryAcquire пытается получить токен без ожидания.
// Возвращает true, если токен получен, false иначе.
func (l *Limiter) TryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Пополнить токены
	l.refill()

	// Проверить, есть ли токены
	if l.tokens >= 1 {
		l.tokens--
		return true
	}

	return false
}

// refill пополняет токены на основе прошедшего времени.
// Должен вызываться под блокировкой мутекса.
func (l *Limiter) refill() {
	now := time.Now()
	elapsed := now.Sub(l.lastRefill).Seconds()

	// Рассчитать, сколько токенов добавить
	tokensToAdd := elapsed * l.refillRate

	// Добавить токены (не превышая максимум)
	l.tokens = min(l.tokens+tokensToAdd, l.maxTokens)
	l.lastRefill = now
}

// timeUntilNextToken рассчитывает время до появления следующего токена.
// Должен вызываться под блокировкой мутекса.
func (l *Limiter) timeUntilNextToken() time.Duration {
	if l.tokens >= 1 {
		return 0
	}

	// Сколько токенов не хватает до 1
	tokensNeeded := 1 - l.tokens

	// Сколько времени потребуется для получения этих токенов
	secondsNeeded := tokensNeeded / l.refillRate

	return time.Duration(secondsNeeded * float64(time.Second))
}

// GetAvailableTokens возвращает текущее количество доступных токенов.
func (l *Limiter) GetAvailableTokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refill()
	return l.tokens
}

// Reset сбрасывает limiter, заполняя корзину токенами полностью.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.tokens = l.maxTokens
	l.lastRefill = time.Now()
}

// String возвращает строковое представление состояния limiter.
func (l *Limiter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refill()
	return fmt.Sprintf(
		"Limiter{tokens: %.2f/%.0f, rate: %.2f/sec}",
		l.tokens,
		l.maxTokens,
		l.refillRate,
	)
}

// min возвращает минимум из двух float64
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// minDuration возвращает минимум из двух time.Duration
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
