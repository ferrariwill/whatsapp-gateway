package slug

import (
	"regexp"
	"strings"
)

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// Normalize gera um slug estável a partir do nome da aplicação (ex.: "Beleza Web" → "beleza_web").
func Normalize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = nonAlphanumeric.ReplaceAllString(name, "_")
	name = strings.Trim(name, "_")
	if name == "" {
		return "app"
	}
	if len(name) > 64 {
		name = strings.TrimRight(name[:64], "_")
	}
	return name
}

var validSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,62}[a-z0-9]$|^[a-z0-9]$`)

// IsValid indica se o slug pode ser usado em state OAuth e lookups.
func IsValid(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return false
	}
	return validSlug.MatchString(value)
}
