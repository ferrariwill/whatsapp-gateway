package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/envutil"
	"github.com/whatsappgetway/gateway/internal/provider"
)

func main() {
	envutil.LoadDotEnv()

	token := strings.TrimSpace(os.Getenv("META_TEST_ACCESS_TOKEN"))
	waba := strings.TrimSpace(os.Getenv("META_TEST_WABA_ID"))
	phoneNumberID := strings.TrimSpace(os.Getenv("META_TEST_PHONE_NUMBER_ID"))
	version := strings.TrimSpace(os.Getenv("META_API_VERSION"))

	fmt.Println("=== Diagnóstico Meta WhatsApp Cloud API (multi-tenant) ===")
	fmt.Printf("META_API_VERSION: %s\n", orDefault(version, "v21.0"))
	fmt.Printf("META_TEST_WABA_ID: %s\n", mask(waba))
	fmt.Printf("META_TEST_ACCESS_TOKEN: %s (len=%d)\n", mask(token), len(token))
	fmt.Printf("META_TEST_PHONE_NUMBER_ID: %s\n", mask(phoneNumberID))

	if token == "" || waba == "" {
		log.Fatal("META_TEST_ACCESS_TOKEN e META_TEST_WABA_ID são obrigatórios no .env para este teste")
	}

	client := &http.Client{Timeout: 20 * time.Second}
	meta := provider.NewMetaProvider(client, version)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fmt.Println("\n--- Teste 1: Listar templates (GET /{WABA_ID}/message_templates) ---")
	templates, err := meta.GetTemplatesStatus(ctx, token, waba)
	if err != nil {
		fmt.Printf("FALHOU: %v\n", err)
	} else {
		fmt.Printf("OK — %d template(s) encontrado(s)\n", len(templates))
		for i, tpl := range templates {
			if i >= 5 {
				fmt.Printf("... e mais %d\n", len(templates)-5)
				break
			}
			fmt.Printf("  • %s [%s] %s (%s)\n", tpl.Name, tpl.Status, tpl.Category, tpl.Language)
		}
	}

	if phoneNumberID == "" {
		fmt.Println("\n--- Teste 2: Envio de template — SKIP (META_TEST_PHONE_NUMBER_ID não configurado) ---")
		return
	}

	fmt.Println("\n--- Teste 2: Envio de template (valida rota de envio) ---")
	_, err = meta.SendAppointmentTemplate(ctx, token, phoneNumberID, "5511999999999", "nome_template_inexistente", []string{"teste"})
	if err != nil {
		fmt.Printf("Resposta esperada de erro (valida rota de envio): %v\n", err)
	} else {
		fmt.Println("OK — envio retornou sucesso (inesperado com template fictício)")
	}
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func mask(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "(vazio)"
	}
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "…" + value[len(value)-4:]
}
