package main

import (
	"fmt"

	"github.com/whatsappgetway/gateway/internal/envutil"
	"github.com/whatsappgetway/gateway/internal/migrate"
)

func runMigrations(dbURL string) error {
	result, err := migrate.Up(dbURL)
	if err != nil {
		return err
	}
	if result.Dirty {
		return fmt.Errorf("migration database is dirty at version %d", result.Version)
	}
	return nil
}

func init() {
	envutil.LoadDotEnv()
}
