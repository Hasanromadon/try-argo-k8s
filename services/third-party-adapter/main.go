package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.Println("Starting Third-Party Adapter Service...")

	// 1. Inisialisasi Konfigurasi (Viper Hot-Reload)
	InitConfig()

	// 2. Inisialisasi Koneksi Database (Untuk Parameter dan Advisory Lock)
	InitDB()

	// 3. Menjalankan gRPC Server secara asinkron
	go StartGRPCServer(AppConfig.Server.GRPCPort)

	// 4. Menjalankan HTTP Server (Webhook Listener) secara asinkron
	go StartHTTPServer(AppConfig.Server.HTTPPort)

	// --- UJI COBA (SIMULASI) SINGLETON TOKEN ---
	// Kita simulasikan pemanggilan token sesaat setelah server menyala.
	// Di environment produksi, ini bisa dipanggil secara periodik (cron)
	// atau dipanggil tepat sebelum gRPC Request ditembakkan ke vendor.
	go func() {
		time.Sleep(2 * time.Second)
		token, err := FetchTokenSafe()
		if err != nil {
			log.Printf("Gagal mendapatkan token: %v\n", err)
		} else {
			log.Printf("Token yang akan digunakan untuk koneksi vendor: %s\n", token)
		}
	}()

	// Tunggu sinyal terminasi dari OS (Kubernetes SIGTERM)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	
	log.Println("Shutting down Third-Party Adapter gracefully...")
	// Cleanup resources (db.Close(), grpcServer.GracefulStop(), dll)
}
