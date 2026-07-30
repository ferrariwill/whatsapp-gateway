package migrate

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	appmigrations "github.com/whatsappgetway/gateway/migrations"
)

// Result resume o estado das migrations após a execução.
type Result struct {
	Version     uint
	Dirty       bool
	Applied     bool
	NoChange    bool
	DatabaseURL string
}

// Up aplica todas as migrations pendentes no banco indicado por dbURL.
func Up(dbURL string) (Result, error) {
	result := Result{DatabaseURL: redactDatabaseURL(dbURL)}

	dbURL = normalizeDatabaseURL(dbURL)

	source, err := iofs.New(appmigrations.FS, ".")
	if err != nil {
		return result, fmt.Errorf("load embedded migrations: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, dbURL)
	if err != nil {
		return result, fmt.Errorf("create migrator: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			log.Printf("close migration source: %v", srcErr)
		}
		if dbErr != nil {
			log.Printf("close migration database: %v", dbErr)
		}
	}()

	versionBefore, dirtyBefore, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return result, fmt.Errorf("read migration version: %w", err)
	}

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			result.Version = versionBefore
			result.Dirty = dirtyBefore
			result.NoChange = true
			return result, nil
		}
		return result, fmt.Errorf("apply migrations: %w", err)
	}

	versionAfter, dirtyAfter, err := m.Version()
	if err != nil {
		return result, fmt.Errorf("read migration version after apply: %w", err)
	}

	result.Version = versionAfter
	result.Dirty = dirtyAfter
	result.Applied = true

	return result, nil
}

func normalizeDatabaseURL(dbURL string) string {
	return strings.Replace(dbURL, "postgresql://", "postgres://", 1)
}

func redactDatabaseURL(dbURL string) string {
	if idx := strings.Index(dbURL, "@"); idx >= 0 {
		if schemeIdx := strings.Index(dbURL, "://"); schemeIdx >= 0 {
			return dbURL[:schemeIdx+3] + "***@" + dbURL[idx+1:]
		}
	}
	return dbURL
}
