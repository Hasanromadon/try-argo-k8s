package main

import (
	"database/sql"
	"errors"
	"log"
	"time"

	_ "github.com/lib/pq"
)

var db *sql.DB

// Lock ID yang unik untuk proses fetch token vendor tertentu
const vendorAuthLockID = 1001

func InitDB() {
	var err error
	db, err = sql.Open("postgres", AppConfig.Database.URL)
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}

	if err = db.Ping(); err != nil {
		log.Printf("Warning: Cannot ping database: %v (Simulasi mungkin belum dinyalakan)", err)
	} else {
		log.Println("Database connected successfully.")
	}
}

// FetchTokenSafe menjamin bahwa hanya ada 1 pod yang melakukan request HTTP ke API Vendor
// untuk mendapatkan token baru. Pod lain akan menunggu atau mengambil dari database.
func FetchTokenSafe() (string, error) {
	// Mulai transaksi untuk advisory lock level transaksi
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback() // Rollback jika ada error, lock otomatis terlepas

	// Coba ambil lock secara eksklusif dan tunggu (blocking)
	log.Println("Mencoba mengambil Advisory Lock...")
	_, err = tx.Exec("SELECT pg_advisory_xact_lock($1)", vendorAuthLockID)
	if err != nil {
		return "", err
	}
	log.Println("Advisory Lock berhasil didapatkan! Memeriksa token di database...")

	// Cek apakah pod lain baru saja menyimpan token yang masih valid
	var token string
	var expiresAt time.Time
	err = tx.QueryRow("SELECT token, expires_at FROM vendor_tokens WHERE vendor_id = 'biller_v1'").Scan(&token, &expiresAt)
	
	if err == nil && time.Now().Before(expiresAt) {
		log.Println("Token valid ditemukan di DB. Menggunakan token yang sudah ada.")
		return token, nil
	}

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	// Token kosong atau kedaluwarsa, lakukan HTTP Call ke Vendor API
	log.Println("Token kedaluwarsa atau belum ada. Menembak API Vendor...")
	
	// SIMULASI HTTP CALL KE VENDOR
	time.Sleep(1 * time.Second)
	newToken := "vendor-token-simulasi-" + time.Now().Format("150405")
	newExpiresAt := time.Now().Add(1 * time.Hour)

	log.Println("Mendapatkan token baru dari Vendor API.")

	// Simpan ke DB agar Pod lain bisa menggunakannya
	_, err = tx.Exec(`
		INSERT INTO vendor_tokens (vendor_id, token, expires_at) 
		VALUES ('biller_v1', $1, $2)
		ON CONFLICT (vendor_id) DO UPDATE SET token = $1, expires_at = $2
	`, newToken, newExpiresAt)
	
	if err != nil {
		return "", err
	}

	// Commit transaksi, yang secara otomatis melepas pg_advisory_xact_lock
	if err = tx.Commit(); err != nil {
		return "", err
	}

	log.Println("Transaksi commit, Advisory Lock dilepas.")
	return newToken, nil
}
