package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config представляет главную конфигурацию приложения.
// Загружается из файла configs/app.yaml с поддержкой переменных окружения.
type Config struct {
	Database DatabaseConfig            `yaml:"database"`
	Engine   EngineConfig              `yaml:"engine"`
	Exchanges map[string]ExchangeConfig `yaml:"exchanges"`
	Logging  LoggingConfig             `yaml:"logging"`
	Metrics  MetricsConfig             `yaml:"metrics"`
}

// DatabaseConfig содержит параметры подключения к PostgreSQL.
type DatabaseConfig struct {
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	User           string `yaml:"user"`
	Password       string `yaml:"password"`
	DBName         string `yaml:"dbname"`
	MaxConnections int    `yaml:"max_connections"`
}

// EngineConfig содержит параметры торгового движка.
type EngineConfig struct {
	NumShards              int           `yaml:"num_shards"`
	WorkersPerShard        int           `yaml:"workers_per_shard"`
	BufferSize             int           `yaml:"buffer_size"`
	MaxConcurrentArbs      int           `yaml:"max_concurrent_arbs"`
	BalanceUpdateInterval  time.Duration `yaml:"balance_update_interval"`
}

// ExchangeConfig содержит параметры подключения к бирже.
type ExchangeConfig struct {
	APIKey     string `yaml:"api_key"`
	APISecret  string `yaml:"api_secret"`
	Passphrase string `yaml:"passphrase,omitempty"` // Только для OKX
	RestURL    string `yaml:"rest_url"`
	WsURL      string `yaml:"ws_url"`
}

// LoggingConfig содержит параметры логирования.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // json, console
	Output string `yaml:"output"` // путь к файлу или stdout
}

// MetricsConfig содержит параметры экспорта метрик Prometheus.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Path    string `yaml:"path"`
}

// Load загружает конфигурацию из YAML файла.
// Поддерживает подстановку переменных окружения в формате ${VAR_NAME}.
func Load(configPath string) (*Config, error) {
	// Прочитать файл
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать конфигурационный файл %s: %w", configPath, err)
	}

	// Подставить переменные окружения
	expandedData := expandEnvVars(string(data))

	// Распарсить YAML
	var config Config
	if err := yaml.Unmarshal([]byte(expandedData), &config); err != nil {
		return nil, fmt.Errorf("не удалось распарсить YAML: %w", err)
	}

	// Валидировать
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("ошибка валидации конфигурации: %w", err)
	}

	return &config, nil
}

// Validate проверяет корректность загруженной конфигурации.
func (c *Config) Validate() error {
	// Проверка БД
	if c.Database.Host == "" {
		return fmt.Errorf("database.host не может быть пустым")
	}
	if c.Database.Port == 0 {
		return fmt.Errorf("database.port не может быть 0")
	}
	if c.Database.User == "" {
		return fmt.Errorf("database.user не может быть пустым")
	}
	if c.Database.Password == "" || strings.HasPrefix(c.Database.Password, "${") {
		return fmt.Errorf("database.password не может быть пустым или нераскрытой переменной окружения")
	}
	if c.Database.DBName == "" {
		return fmt.Errorf("database.dbname не может быть пустым")
	}
	if c.Database.MaxConnections < 1 {
		return fmt.Errorf("database.max_connections должен быть >= 1")
	}

	// Проверка движка
	if c.Engine.NumShards < 1 {
		return fmt.Errorf("engine.num_shards должен быть >= 1")
	}
	if c.Engine.WorkersPerShard < 1 {
		return fmt.Errorf("engine.workers_per_shard должен быть >= 1")
	}
	if c.Engine.BufferSize < 100 {
		return fmt.Errorf("engine.buffer_size должен быть >= 100")
	}
	if c.Engine.MaxConcurrentArbs < 1 {
		return fmt.Errorf("engine.max_concurrent_arbs должен быть >= 1")
	}
	// Валидация интервала обновления балансов (Requirements.md: диапазон 30s-300s)
	// Защита от panic в time.NewTicker при нулевом/отрицательном значении
	if c.Engine.BalanceUpdateInterval < 30*time.Second {
		return fmt.Errorf("engine.balance_update_interval должен быть >= 30s, получено: %v", c.Engine.BalanceUpdateInterval)
	}
	if c.Engine.BalanceUpdateInterval > 300*time.Second {
		return fmt.Errorf("engine.balance_update_interval должен быть <= 300s, получено: %v", c.Engine.BalanceUpdateInterval)
	}

	// Проверка бирж
	for name, exchCfg := range c.Exchanges {
		if exchCfg.APIKey == "" || strings.HasPrefix(exchCfg.APIKey, "${") {
			return fmt.Errorf("exchanges.%s.api_key не может быть пустым или нераскрытой переменной окружения", name)
		}
		if exchCfg.APISecret == "" || strings.HasPrefix(exchCfg.APISecret, "${") {
			return fmt.Errorf("exchanges.%s.api_secret не может быть пустым или нераскрытой переменной окружения", name)
		}
		if exchCfg.RestURL == "" {
			return fmt.Errorf("exchanges.%s.rest_url не может быть пустым", name)
		}
		if exchCfg.WsURL == "" {
			return fmt.Errorf("exchanges.%s.ws_url не может быть пустым", name)
		}
		// OKX требует обязательный passphrase для аутентификации
		if name == "okx" {
			if exchCfg.Passphrase == "" || strings.HasPrefix(exchCfg.Passphrase, "${") {
				return fmt.Errorf("exchanges.okx.passphrase обязателен для OKX и не может быть пустым или нераскрытой переменной окружения")
			}
		}
	}

	// Проверка логирования
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.Logging.Level] {
		return fmt.Errorf("logging.level должен быть одним из: debug, info, warn, error")
	}
	validFormats := map[string]bool{"json": true, "console": true}
	if !validFormats[c.Logging.Format] {
		return fmt.Errorf("logging.format должен быть одним из: json, console")
	}
	if c.Logging.Output == "" {
		return fmt.Errorf("logging.output не может быть пустым")
	}

	return nil
}

// expandEnvVars заменяет ${VAR_NAME} на значения переменных окружения.
func expandEnvVars(data string) string {
	return os.Expand(data, func(key string) string {
		value := os.Getenv(key)
		if value == "" {
			// Если переменная не установлена, оставляем как есть
			return "${" + key + "}"
		}
		return value
	})
}

// GetDSN возвращает строку подключения к PostgreSQL в формате для database/sql.
// Корректно экранирует специальные символы в пароле.
func (d *DatabaseConfig) GetDSN() string {
	// Для key=value формата PostgreSQL нужно заключить значения со спецсимволами в одинарные кавычки
	// и экранировать одинарные кавычки внутри значения
	password := d.Password
	needsQuotes := strings.ContainsAny(password, " '\\=")

	if needsQuotes {
		// Экранировать одинарные кавычки и обернуть в кавычки
		password = "'" + strings.ReplaceAll(password, "'", "\\'") + "'"
	}

	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		d.Host,
		d.Port,
		d.User,
		password,
		d.DBName,
	)
}
