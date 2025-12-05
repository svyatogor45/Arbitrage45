package db

import (
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // PostgreSQL драйвер для миграций
	_ "github.com/golang-migrate/migrate/v4/source/file"       // Источник миграций из файлов
)

// RunMigrations применяет все миграции из директории migrationsPath.
// Параметры:
//   - dsn: строка подключения к PostgreSQL
//   - migrationsPath: путь к директории с миграциями (например, "file://internal/db/migrations")
//
// Возвращает ошибку, если миграции не применились.
func RunMigrations(dsn, migrationsPath string) error {
	m, err := migrate.New(migrationsPath, dsn)
	if err != nil {
		return fmt.Errorf("не удалось создать экземпляр migrate: %w", err)
	}
	defer m.Close()

	// Применить все миграции
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("не удалось применить миграции: %w", err)
	}

	return nil
}

// RollbackMigrations откатывает одну последнюю миграцию.
// Параметры:
//   - dsn: строка подключения к PostgreSQL
//   - migrationsPath: путь к директории с миграциями
//
// Возвращает ошибку, если откат не удался.
func RollbackMigrations(dsn, migrationsPath string) error {
	m, err := migrate.New(migrationsPath, dsn)
	if err != nil {
		return fmt.Errorf("не удалось создать экземпляр migrate: %w", err)
	}
	defer m.Close()

	// Откатить одну миграцию
	if err := m.Steps(-1); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("не удалось откатить миграцию: %w", err)
	}

	return nil
}

// MigrationVersion возвращает текущую версию миграции.
// Параметры:
//   - dsn: строка подключения к PostgreSQL
//   - migrationsPath: путь к директории с миграциями
//
// Возвращает номер версии и ошибку.
func MigrationVersion(dsn, migrationsPath string) (uint, bool, error) {
	m, err := migrate.New(migrationsPath, dsn)
	if err != nil {
		return 0, false, fmt.Errorf("не удалось создать экземпляр migrate: %w", err)
	}
	defer m.Close()

	version, dirty, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return 0, false, fmt.Errorf("не удалось получить версию миграции: %w", err)
	}

	// Если миграций ещё не было, вернуть 0
	if err == migrate.ErrNilVersion {
		return 0, false, nil
	}

	return version, dirty, nil
}
