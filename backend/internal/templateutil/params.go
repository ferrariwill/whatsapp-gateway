package templateutil

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var bodyPlaceholderRE = regexp.MustCompile(`\{\{(\d+)\}\}`)

type componentWithText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ExpectedBodyParamCount deriva N a partir dos placeholders {{1}}…{{n}} no BODY.
// Usa o maior índice encontrado; HEADER/BUTTONS são ignorados (gate de variables[] é body-only).
func ExpectedBodyParamCount(componentsJSON []byte) int {
	if len(componentsJSON) == 0 {
		return 0
	}
	var components []componentWithText
	if err := json.Unmarshal(componentsJSON, &components); err != nil {
		return 0
	}
	max := 0
	for _, c := range components {
		if !strings.EqualFold(strings.TrimSpace(c.Type), "BODY") {
			continue
		}
		for _, m := range bodyPlaceholderRE.FindAllStringSubmatch(c.Text, -1) {
			if len(m) < 2 {
				continue
			}
			n, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			if n > max {
				max = n
			}
		}
	}
	return max
}

// NormalizeTemplateStatus uppercases e mapeia valores conhecidos da Meta.
func NormalizeTemplateStatus(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	switch s {
	case "APPROVED", "PENDING", "REJECTED", "PAUSED", "DISABLED":
		return s
	default:
		return s
	}
}
