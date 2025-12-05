package bybit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"arbitrage-terminal/internal/exchanges"
	"arbitrage-terminal/pkg/ratelimit"
	"go.uber.org/zap"
)

// =============================================================================
// Константы REST клиента
// =============================================================================

const (
	// DefaultRecvWindow — окно приёма запросов (мс).
	// Bybit отклонит запрос, если он придёт позже чем timestamp + recvWindow.
	DefaultRecvWindow = 5000

	// DefaultTimeout — таймаут HTTP запросов.
	DefaultTimeout = 10 * time.Second

	// RateLimitRequests — безопасный лимит запросов (50% от 120 req/min).
	RateLimitRequests = 60

	// RateLimitBurst — максимальный burst для rate limiter.
	RateLimitBurst = 10

	// RateLimitPeriod — период rate limit.
	RateLimitPeriod = time.Minute
)

// =============================================================================
// RestClient — REST клиент для Bybit API
// =============================================================================

// RestClient реализует взаимодействие с Bybit REST API V5.
// Поддерживает подпись запросов HMAC-SHA256 и rate limiting.
type RestClient struct {
	httpClient  *http.Client           // HTTP клиент
	baseURL     string                 // Базовый URL API
	apiKey      string                 // API ключ
	apiSecret   string                 // API секрет
	recvWindow  int                    // Окно приёма (мс)
	rateLimiter *ratelimit.Limiter     // Rate limiter
	logger      *zap.Logger            // Логгер
}

// RestClientConfig содержит конфигурацию для создания REST клиента.
type RestClientConfig struct {
	BaseURL    string        // Базовый URL (по умолчанию RestBaseURL)
	APIKey     string        // API ключ (обязательный)
	APISecret  string        // API секрет (обязательный)
	RecvWindow int           // Окно приёма (по умолчанию DefaultRecvWindow)
	Timeout    time.Duration // Таймаут HTTP (по умолчанию DefaultTimeout)
	Logger     *zap.Logger   // Логгер (опционально)
}

// NewRestClient создаёт новый REST клиент для Bybit API.
//
// Параметры:
//   - cfg: конфигурация клиента
//
// Возвращает ошибку, если API ключи не указаны.
func NewRestClient(cfg RestClientConfig) (*RestClient, error) {
	// Валидация обязательных параметров
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("bybit: API key is required")
	}
	if cfg.APISecret == "" {
		return nil, fmt.Errorf("bybit: API secret is required")
	}

	// Установка значений по умолчанию
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = RestBaseURL
	}

	recvWindow := cfg.RecvWindow
	if recvWindow == 0 {
		recvWindow = DefaultRecvWindow
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	// Создание HTTP клиента с оптимизированными настройками
	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Создание rate limiter (60 запросов в минуту, burst = 10)
	limiter := ratelimit.NewLimiter(RateLimitRequests, RateLimitBurst, RateLimitPeriod)

	return &RestClient{
		httpClient:  httpClient,
		baseURL:     baseURL,
		apiKey:      cfg.APIKey,
		apiSecret:   cfg.APISecret,
		recvWindow:  recvWindow,
		rateLimiter: limiter,
		logger:      logger,
	}, nil
}

// =============================================================================
// Подпись запросов
// =============================================================================

// sign создаёт HMAC-SHA256 подпись для запроса к Bybit API V5.
//
// Формат подписи: HMAC-SHA256(timestamp + apiKey + recvWindow + payload)
// где payload — это query string для GET или JSON body для POST.
func (c *RestClient) sign(timestamp int64, payload string) string {
	// Формируем строку для подписи
	signStr := fmt.Sprintf("%d%s%d%s", timestamp, c.apiKey, c.recvWindow, payload)

	// Вычисляем HMAC-SHA256
	h := hmac.New(sha256.New, []byte(c.apiSecret))
	h.Write([]byte(signStr))

	return hex.EncodeToString(h.Sum(nil))
}

// =============================================================================
// Выполнение запросов
// =============================================================================

// doRequest выполняет HTTP запрос к Bybit API с подписью и rate limiting.
//
// Параметры:
//   - ctx: контекст для отмены запроса
//   - method: HTTP метод (GET, POST)
//   - endpoint: эндпоинт API (например, "/v5/order/create")
//   - params: параметры запроса (для GET — query, для POST — body)
//   - result: указатель на структуру для десериализации ответа
//
// Возвращает ошибку типа *exchanges.ExchangeError при ошибках API.
func (c *RestClient) doRequest(ctx context.Context, method, endpoint string, params map[string]interface{}, result interface{}) error {
	// Ждём токен rate limiter
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return &exchanges.ExchangeError{
			Type:    exchanges.ErrorTypeRateLimit,
			Message: fmt.Sprintf("rate limit wait cancelled: %v", err),
			Retry:   false,
		}
	}

	// Получаем текущий timestamp
	timestamp := time.Now().UnixMilli()

	// Формируем URL и тело запроса
	fullURL := c.baseURL + endpoint
	var body io.Reader
	var payload string

	if method == http.MethodGet {
		// Для GET запросов параметры идут в query string
		if len(params) > 0 {
			query := c.buildQueryString(params)
			fullURL += "?" + query
			payload = query
		}
	} else {
		// Для POST запросов параметры идут в JSON body
		if len(params) > 0 {
			jsonData, err := json.Marshal(params)
			if err != nil {
				return fmt.Errorf("failed to marshal request body: %w", err)
			}
			body = bytes.NewReader(jsonData)
			payload = string(jsonData)
		}
	}

	// Создаём подпись
	signature := c.sign(timestamp, payload)

	// Создаём HTTP запрос
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Устанавливаем заголовки
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BAPI-API-KEY", c.apiKey)
	req.Header.Set("X-BAPI-TIMESTAMP", strconv.FormatInt(timestamp, 10))
	req.Header.Set("X-BAPI-SIGN", signature)
	req.Header.Set("X-BAPI-RECV-WINDOW", strconv.Itoa(c.recvWindow))

	// Логируем запрос (без секретных данных)
	c.logger.Debug("bybit REST request",
		zap.String("method", method),
		zap.String("endpoint", endpoint),
		zap.Int64("timestamp", timestamp),
	)

	// Выполняем запрос
	startTime := time.Now()
	resp, err := c.httpClient.Do(req)
	duration := time.Since(startTime)

	if err != nil {
		c.logger.Error("bybit REST request failed",
			zap.String("endpoint", endpoint),
			zap.Duration("duration", duration),
			zap.Error(err),
		)
		return &exchanges.ExchangeError{
			Type:    exchanges.ErrorTypeNetworkError,
			Message: fmt.Sprintf("HTTP request failed: %v", err),
			Retry:   true,
		}
	}
	defer resp.Body.Close()

	// Читаем тело ответа
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// Логируем ответ
	c.logger.Debug("bybit REST response",
		zap.String("endpoint", endpoint),
		zap.Int("status", resp.StatusCode),
		zap.Duration("duration", duration),
	)

	// Парсим базовый ответ для проверки ошибок
	var baseResp APIResponseRaw
	if err := json.Unmarshal(respBody, &baseResp); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	// Проверяем код возврата
	if baseResp.RetCode != RetCodeSuccess {
		return c.handleAPIError(baseResp.RetCode, baseResp.RetMsg)
	}

	// Десериализуем результат, если указан
	if result != nil {
		// Парсим полный ответ с нужным типом result
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to parse result: %w", err)
		}
	}

	return nil
}

// buildQueryString создаёт query string из параметров.
// Параметры сортируются по ключу для консистентности.
func (c *RestClient) buildQueryString(params map[string]interface{}) string {
	if len(params) == 0 {
		return ""
	}

	// Собираем ключи и сортируем
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Формируем query string
	var parts []string
	for _, k := range keys {
		v := params[k]
		var strVal string
		switch val := v.(type) {
		case string:
			strVal = val
		case int:
			strVal = strconv.Itoa(val)
		case int64:
			strVal = strconv.FormatInt(val, 10)
		case float64:
			strVal = strconv.FormatFloat(val, 'f', -1, 64)
		case bool:
			strVal = strconv.FormatBool(val)
		default:
			strVal = fmt.Sprintf("%v", val)
		}
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(strVal))
	}

	return strings.Join(parts, "&")
}

// handleAPIError преобразует код ошибки Bybit в ExchangeError.
func (c *RestClient) handleAPIError(code int, msg string) *exchanges.ExchangeError {
	errType := exchanges.ErrorTypeUnknown
	retry := false

	switch code {
	case ErrCodeRateLimit:
		errType = exchanges.ErrorTypeRateLimit
		retry = true
	case ErrCodeInsufficientBalance:
		errType = exchanges.ErrorTypeInsufficientMargin
		retry = false
	case ErrCodeInvalidSymbol:
		errType = exchanges.ErrorTypeInvalidSymbol
		retry = false
	case ErrCodeInvalidQty:
		errType = exchanges.ErrorTypeInvalidQuantity
		retry = false
	case ErrCodeLeverageNotChanged:
		// Плечо уже установлено — это не ошибка, можно игнорировать
		errType = exchanges.ErrorTypeUnknown
		retry = false
	default:
		// Для неизвестных ошибок проверяем, стоит ли повторять
		if code >= 10000 && code < 20000 {
			// Системные ошибки — можно повторить
			retry = true
		}
	}

	c.logger.Warn("bybit API error",
		zap.Int("code", code),
		zap.String("message", msg),
		zap.String("type", string(errType)),
		zap.Bool("retry", retry),
	)

	return &exchanges.ExchangeError{
		Type:    errType,
		Code:    code,
		Message: msg,
		Retry:   retry,
	}
}

// =============================================================================
// API методы — Ордера
// =============================================================================

// PlaceOrder выставляет ордер на Bybit.
//
// Параметры:
//   - ctx: контекст для отмены
//   - req: параметры ордера
//
// Возвращает ID созданного ордера или ошибку.
func (c *RestClient) PlaceOrder(ctx context.Context, req *PlaceOrderRequest) (*PlaceOrderResult, error) {
	// Валидация обязательных полей
	if req.Symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	if req.Side == "" {
		return nil, fmt.Errorf("side is required")
	}
	if req.OrderType == "" {
		return nil, fmt.Errorf("orderType is required")
	}
	if req.Qty == "" {
		return nil, fmt.Errorf("qty is required")
	}

	// Устанавливаем category по умолчанию
	if req.Category == "" {
		req.Category = CategoryLinear
	}

	// Формируем параметры запроса
	params := map[string]interface{}{
		"category":    req.Category,
		"symbol":      req.Symbol,
		"side":        req.Side,
		"orderType":   req.OrderType,
		"qty":         req.Qty,
		"positionIdx": req.PositionIdx,
	}

	// Добавляем опциональные параметры
	if req.Price != "" {
		params["price"] = req.Price
	}
	if req.TimeInForce != "" {
		params["timeInForce"] = req.TimeInForce
	}
	if req.OrderLinkId != "" {
		params["orderLinkId"] = req.OrderLinkId
	}
	if req.ReduceOnly {
		params["reduceOnly"] = req.ReduceOnly
	}

	// Выполняем запрос
	var resp APIResponse[PlaceOrderResult]
	if err := c.doRequest(ctx, http.MethodPost, EndpointPlaceOrder, params, &resp); err != nil {
		return nil, err
	}

	c.logger.Info("bybit order placed",
		zap.String("symbol", req.Symbol),
		zap.String("side", req.Side),
		zap.String("qty", req.Qty),
		zap.String("orderId", resp.Result.OrderID),
	)

	return &resp.Result, nil
}

// CancelOrder отменяет ордер на Bybit.
//
// Параметры:
//   - ctx: контекст для отмены
//   - symbol: символ торговой пары
//   - orderID: ID ордера (или orderLinkId)
func (c *RestClient) CancelOrder(ctx context.Context, symbol, orderID string) error {
	params := map[string]interface{}{
		"category": CategoryLinear,
		"symbol":   symbol,
		"orderId":  orderID,
	}

	if err := c.doRequest(ctx, http.MethodPost, EndpointCancelOrder, params, nil); err != nil {
		return err
	}

	c.logger.Info("bybit order cancelled",
		zap.String("symbol", symbol),
		zap.String("orderId", orderID),
	)

	return nil
}

// GetOrders возвращает список активных ордеров.
//
// Параметры:
//   - ctx: контекст для отмены
//   - symbol: символ торговой пары (опционально, пустая строка = все)
func (c *RestClient) GetOrders(ctx context.Context, symbol string) (*GetOrdersResult, error) {
	params := map[string]interface{}{
		"category": CategoryLinear,
	}
	if symbol != "" {
		params["symbol"] = symbol
	}

	var resp APIResponse[GetOrdersResult]
	if err := c.doRequest(ctx, http.MethodGet, EndpointGetOrders, params, &resp); err != nil {
		return nil, err
	}

	return &resp.Result, nil
}

// =============================================================================
// API методы — Баланс
// =============================================================================

// GetBalance возвращает баланс указанной монеты.
//
// Параметры:
//   - ctx: контекст для отмены
//   - coin: название монеты (например, "USDT")
//
// Возвращает информацию о балансе или ошибку.
func (c *RestClient) GetBalance(ctx context.Context, coin string) (*CoinInfo, error) {
	params := map[string]interface{}{
		"accountType": "UNIFIED", // Unified Trading Account
	}
	if coin != "" {
		params["coin"] = coin
	}

	var resp APIResponse[GetWalletBalanceResult]
	if err := c.doRequest(ctx, http.MethodGet, EndpointGetWalletBalance, params, &resp); err != nil {
		return nil, err
	}

	// Ищем нужную монету в ответе
	for _, account := range resp.Result.List {
		for _, coinInfo := range account.Coin {
			if coinInfo.Coin == coin {
				return &coinInfo, nil
			}
		}
	}

	return nil, fmt.Errorf("coin %s not found in balance", coin)
}

// GetWalletBalance возвращает полный баланс кошелька.
//
// Параметры:
//   - ctx: контекст для отмены
func (c *RestClient) GetWalletBalance(ctx context.Context) (*GetWalletBalanceResult, error) {
	params := map[string]interface{}{
		"accountType": "UNIFIED",
	}

	var resp APIResponse[GetWalletBalanceResult]
	if err := c.doRequest(ctx, http.MethodGet, EndpointGetWalletBalance, params, &resp); err != nil {
		return nil, err
	}

	return &resp.Result, nil
}

// =============================================================================
// API методы — Плечо
// =============================================================================

// SetLeverage устанавливает плечо для указанного символа.
//
// Параметры:
//   - ctx: контекст для отмены
//   - symbol: символ торговой пары
//   - leverage: значение плеча (например, 10)
//
// Примечание: плечо устанавливается одинаково для long и short позиций.
// Если плечо уже установлено на это значение, возвращается nil (не ошибка).
func (c *RestClient) SetLeverage(ctx context.Context, symbol string, leverage int) error {
	leverageStr := strconv.Itoa(leverage)

	params := map[string]interface{}{
		"category":     CategoryLinear,
		"symbol":       symbol,
		"buyLeverage":  leverageStr,
		"sellLeverage": leverageStr,
	}

	err := c.doRequest(ctx, http.MethodPost, EndpointSetLeverage, params, nil)
	if err != nil {
		// Если плечо уже установлено — это не ошибка
		if exchErr, ok := err.(*exchanges.ExchangeError); ok {
			if exchErr.Code == ErrCodeLeverageNotChanged {
				c.logger.Debug("bybit leverage already set",
					zap.String("symbol", symbol),
					zap.Int("leverage", leverage),
				)
				return nil
			}
		}
		return err
	}

	c.logger.Info("bybit leverage set",
		zap.String("symbol", symbol),
		zap.Int("leverage", leverage),
	)

	return nil
}

// =============================================================================
// API методы — Позиции
// =============================================================================

// GetPositions возвращает список открытых позиций.
//
// Параметры:
//   - ctx: контекст для отмены
//   - symbol: символ торговой пары (опционально, пустая строка = все)
func (c *RestClient) GetPositions(ctx context.Context, symbol string) (*GetPositionsResult, error) {
	params := map[string]interface{}{
		"category": CategoryLinear,
	}
	if symbol != "" {
		params["symbol"] = symbol
	}

	var resp APIResponse[GetPositionsResult]
	if err := c.doRequest(ctx, http.MethodGet, EndpointGetPositions, params, &resp); err != nil {
		return nil, err
	}

	return &resp.Result, nil
}

// GetPosition возвращает позицию по указанному символу.
//
// Параметры:
//   - ctx: контекст для отмены
//   - symbol: символ торговой пары
//
// Возвращает nil, nil если позиция не найдена.
func (c *RestClient) GetPosition(ctx context.Context, symbol string) (*PositionInfo, error) {
	result, err := c.GetPositions(ctx, symbol)
	if err != nil {
		return nil, err
	}

	for _, pos := range result.List {
		if pos.Symbol == symbol {
			// Проверяем, что позиция реально открыта (size > 0)
			size, _ := ParseFloat(pos.Size)
			if size > 0 {
				return &pos, nil
			}
		}
	}

	return nil, nil // Позиция не найдена
}

// =============================================================================
// API методы — Информация об инструментах
// =============================================================================

// GetInstruments возвращает информацию о торговых инструментах.
//
// Параметры:
//   - ctx: контекст для отмены
//   - symbol: символ (опционально, пустая строка = все)
func (c *RestClient) GetInstruments(ctx context.Context, symbol string) (*GetInstrumentsResult, error) {
	params := map[string]interface{}{
		"category": CategoryLinear,
	}
	if symbol != "" {
		params["symbol"] = symbol
	}

	var resp APIResponse[GetInstrumentsResult]
	if err := c.doRequest(ctx, http.MethodGet, EndpointGetInstruments, params, &resp); err != nil {
		return nil, err
	}

	return &resp.Result, nil
}

// GetInstrumentInfo возвращает информацию о конкретном инструменте.
//
// Параметры:
//   - ctx: контекст для отмены
//   - symbol: символ торговой пары
//
// Возвращает информацию о минимумах, шагах цены/объёма и т.д.
func (c *RestClient) GetInstrumentInfo(ctx context.Context, symbol string) (*InstrumentInfo, error) {
	result, err := c.GetInstruments(ctx, symbol)
	if err != nil {
		return nil, err
	}

	for _, inst := range result.List {
		if inst.Symbol == symbol {
			return &inst, nil
		}
	}

	return nil, fmt.Errorf("instrument %s not found", symbol)
}

// =============================================================================
// Вспомогательные методы
// =============================================================================

// GetRateLimiter возвращает rate limiter для внешнего использования.
func (c *RestClient) GetRateLimiter() *ratelimit.Limiter {
	return c.rateLimiter
}

// SetLogger устанавливает логгер.
func (c *RestClient) SetLogger(logger *zap.Logger) {
	c.logger = logger
}

// Ping проверяет доступность API (через запрос времени сервера).
func (c *RestClient) Ping(ctx context.Context) error {
	// Используем публичный эндпоинт для проверки
	params := map[string]interface{}{
		"category": CategoryLinear,
		"symbol":   "BTCUSDT",
	}

	var resp APIResponse[GetInstrumentsResult]
	return c.doRequest(ctx, http.MethodGet, EndpointGetInstruments, params, &resp)
}
