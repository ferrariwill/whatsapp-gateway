package envutil

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv carrega variáveis de arquivos .env locais sem sobrescrever as já definidas.
func LoadDotEnv() {
	for _, path := range dotEnvCandidates() {
		if err := loadDotEnvFile(path); err == nil {
			return
		}
	}
}

func dotEnvCandidates() []string {
	candidates := []string{".env", filepath.Join("..", ".env")}
	if workspace := strings.TrimSpace(os.Getenv("WORKSPACE_FOLDER")); workspace != "" {
		candidates = append(candidates, filepath.Join(workspace, ".env"))
	}
	return candidates
}

func loadDotEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)

		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, value)
	}

	return scanner.Err()
}

// ResolveDatabaseURL retorna DATABASE_URL ou monta a partir de DB_* do .env.
func ResolveDatabaseURL() (string, error) {
	if dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL")); dbURL != "" {
		return dbURL, nil
	}

	host := envOrDefault("DB_HOST", "localhost")
	port := envOrDefault("DB_PORT", "5433")
	user := envOrDefault("DB_USER", "gateway")
	password := os.Getenv("DB_PASSWORD")
	dbName := envOrDefault("DB_NAME", "whatsapp_gateway")

	if password == "" {
		return "", fmt.Errorf("DATABASE_URL is required (or set DB_HOST, DB_PORT, DB_USER, DB_PASSWORD and DB_NAME)")
	}

	return fmt.Sprintf(
		"postgresql://%s:%s@%s:%s/%s?sslmode=disable",
		user,
		password,
		host,
		port,
		dbName,
	), nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
