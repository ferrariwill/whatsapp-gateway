package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/whatsappgetway/gateway/pkg/whatsapp"
)

// Exemplo de integração: sistema de agendamentos disparando confirmação via Gateway.
func main() {
	baseURL := envOrDefault("GATEWAY_BASE_URL", "https://seu-gateway.onrender.com")
	apiKey := os.Getenv("GATEWAY_API_KEY")
	if apiKey == "" {
		log.Fatal("GATEWAY_API_KEY is required")
	}

	client := whatsapp.NewClient(baseURL, apiKey)
	client.TemplateName = "lembrete_agendamento"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := client.SendAppointmentConfirmation(
		ctx,
		"45",
		"apt-2026-0616-001",
		"5511999887766",
		[]string{"Maria Silva", "16/06/2026", "14:30"},
	)
	if err != nil {
		log.Fatalf("send appointment confirmation: %v", err)
	}

	log.Println("confirmação de agendamento enviada com sucesso")
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
