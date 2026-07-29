package main

import (
	"log"
	"os"

	"github.com/whatsappgetway/gateway/internal/envutil"
	"github.com/whatsappgetway/gateway/internal/migrate"
)

func main() {
	envutil.LoadDotEnv()

	dbURL, err := envutil.ResolveDatabaseURL()
	if err != nil {
		log.Fatal(err)
	}

	result, err := migrate.Up(dbURL)
	if err != nil {
		log.Fatalf("run migrations: %v", err)
	}

	switch {
	case result.Dirty:
		log.Fatalf("migration database is dirty at version %d — corrija manualmente antes de continuar", result.Version)
	case result.NoChange:
		log.Printf("no pending migrations (database already at version %d) — %s", result.Version, result.DatabaseURL)
	case result.Applied:
		log.Printf("migrations applied successfully (version %d) — %s", result.Version, result.DatabaseURL)
	default:
		log.Printf("migrations finished (version %d) — %s", result.Version, result.DatabaseURL)
	}

	os.Exit(0)
}
