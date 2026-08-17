package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

// WebhookResponse format dari vendor
type WebhookResponse struct {
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
	Message       string `json:"message"`
}

func StartHTTPServer(port int) {
	mux := http.NewServeMux()

	// 1. Endpoint untuk Webhook / Callback dari Vendor (Jalur Eksternal Publik)
	mux.HandleFunc("/api/v1/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var payload WebhookResponse
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		log.Printf("[WEBHOOK IN] Received update for TX: %s, Status: %s\n", payload.TransactionID, payload.Status)
		
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Webhook processed successfully")
	})

	// 2. Endpoint Khusus untuk IBM ACE (Jalur Internal Private)
	mux.HandleFunc("/api/v1/internal/transfer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		log.Println("[IBM ACE IN] Menerima instruksi transfer HTTP dari IBM ACE ESB.")
		
		// Proses injeksi token dan penerusan transaksi ke vendor dilakukan di sini
		// secara identik dengan alur gRPC.
		
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"status": "ACCEPTED", "message": "Instruction received from IBM ACE"}`)
	})

	// 3. Health Check untuk Kubernetes
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "OK")
	})

	addr := fmt.Sprintf(":%d", port)
	log.Printf("HTTP Server (Webhook & IBM ACE) listening on %s\n", addr)
	
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}
