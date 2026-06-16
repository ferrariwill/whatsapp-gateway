package main

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

func runMigrations(dbURL string) error {
	dbURL = normalizeMigrateDatabaseURL(dbURL)

	source, err := iofs.New(appmigrations.FS, ".")
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, dbURL)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
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

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}

	return nil
}

// golang-migrate registra o driver como "postgres://"; normaliza postgresql:// do pgx.
func normalizeMigrateDatabaseURL(dbURL string) string {
	return strings.Replace(dbURL, "postgresql://", "postgres://", 1)
}
