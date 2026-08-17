package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"banking-microservices/pb"
	adapterpb "banking-microservices/pkg/pb"
	"banking-microservices/pkg/logger"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	_ "github.com/lib/pq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	// Prometheus metrics
	grpcRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_requests_total",
			Help: "Total gRPC requests processed.",
		},
		[]string{"service", "method", "status_code"},
	)
	grpcRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_request_duration_seconds",
			Help:    "gRPC request duration histogram.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"service", "method", "status_code"},
	)
)

type server struct {
	pb.UnimplementedTransferServiceServer
	db            *sql.DB
	acctClient    pb.AccountServiceClient
	adapterClient adapterpb.VendorAdapterServiceClient
}

// GenerateTxnID creates a unique transaction reference number
func GenerateTxnID() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return fmt.Sprintf("txn-tf-%d-%04d", time.Now().Unix(), r.Intn(10000))
}

func (s *server) Transfer(ctx context.Context, req *pb.TransferRequest) (*pb.TransferResponse, error) {
	start := time.Now()
	method := "Transfer"
	log := slog.Default()

	correlationID, _ := ctx.Value(logger.CorrelationIDKey).(string)
	channelID, _ := ctx.Value(logger.ChannelIDKey).(string)

	log.InfoContext(ctx, "Starting fund transfer process",
		slog.String("source", req.GetSourceAccount()),
		slog.String("target", req.GetTargetAccount()),
		slog.Int64("amount", req.GetAmount()),
	)

	txnID := GenerateTxnID()

	// Forward trace context via gRPC outgoing metadata
	md := metadata.Pairs("x-correlation-id", correlationID, "x-channel-id", channelID)
	outCtx := metadata.NewOutgoingContext(ctx, md)

	// Step 1: Debit source account
	log.InfoContext(ctx, "Initiating debit on source account", slog.String("source", req.GetSourceAccount()))
	debitRes, err := s.acctClient.Debit(outCtx, &pb.DebitRequest{
		AccountNumber: req.GetSourceAccount(),
		Amount:        req.GetAmount(),
		TransactionId: txnID,
		Description:   fmt.Sprintf("Debit for transfer %s", txnID),
	})
	if err != nil {
		log.ErrorContext(ctx, "Failed to call debit API on Account Service", slog.String("error", err.Error()))
		s.recordTransfer(ctx, txnID, req, "FAILED_NETWORK_DEBIT")
		grpcRequestsTotal.WithLabelValues("transfer-service", method, "InternalError").Inc()
		return nil, status.Errorf(status.Code(err), "debit call failed")
	}

	if !debitRes.GetSuccess() {
		log.WarnContext(ctx, "Debit declined by Account Service", slog.String("reason", debitRes.GetMessage()))
		s.recordTransfer(ctx, txnID, req, "DECLINED_"+debitRes.GetMessage())
		grpcRequestsTotal.WithLabelValues("transfer-service", method, "Declined").Inc()
		return &pb.TransferResponse{
			TransactionId: txnID,
			Success:       false,
			Message:       fmt.Sprintf("debit declined: %s", debitRes.GetMessage()),
		}, nil
	}

	// Step 2: Kirim instruksi ke Vendor (via Third-Party Adapter)
	log.InfoContext(ctx, "Mengirim instruksi transfer ke Vendor via Adapter", slog.String("target", req.GetTargetAccount()))
	adapterRes, err := s.adapterClient.ExecuteTransfer(outCtx, &adapterpb.TransferRequest{
		TransactionId: txnID,
		SourceAccount: req.GetSourceAccount(),
		TargetAccount: req.GetTargetAccount(),
		Amount:        float64(req.GetAmount()),
		BankCode:      "BCA", // Hardcode for testing
		Narration:     "Transfer via gRPC Adapter",
	})

	if err != nil || adapterRes.GetStatus() != adapterpb.TransferStatus_SUCCESS {
		// SAGA COMPENSATING TRANSACTION: Reversal/Refund if vendor fails
		errMsg := "unknown adapter error"
		if err != nil {
			errMsg = err.Error()
		} else {
			errMsg = adapterRes.GetErrorMessage()
		}
		log.ErrorContext(ctx, "Vendor transfer failed, initiating automatic reversal/refund", slog.String("error", errMsg))

		refundRes, refundErr := s.acctClient.Credit(outCtx, &pb.CreditRequest{
			AccountNumber: req.GetSourceAccount(),
			Amount:        req.GetAmount(),
			TransactionId: txnID + "-refund",
			Description:   fmt.Sprintf("Reversal refund for transfer %s", txnID),
		})
		if refundErr != nil || !refundRes.GetSuccess() {
			log.ErrorContext(ctx, "CRITICAL ERROR: AUTOMATIC REVERSAL/REFUND FAILED - MANUAL INTERVENTION REQUIRED", 
				slog.String("source", req.GetSourceAccount()), 
				slog.Int64("amount", req.GetAmount()),
			)
			s.recordTransfer(ctx, txnID, req, "CRITICAL_REVERSAL_FAILED")
		} else {
			log.InfoContext(ctx, "Automatic reversal/refund completed successfully", slog.String("source", req.GetSourceAccount()))
			s.recordTransfer(ctx, txnID, req, "REVERSED_"+errMsg)
		}

		grpcRequestsTotal.WithLabelValues("transfer-service", method, "Reversed").Inc()
		return &pb.TransferResponse{
			TransactionId: txnID,
			Success:       false,
			Message:       "transfer failed at vendor, funds reversed",
		}, nil
	}

	// Step 3: Record successful transfer in local database
	s.recordTransfer(ctx, txnID, req, "SUCCESS")

	// Step 4: [Asynchronous] Send transaction log to Java MQ Adapter for Big Data
	go func(transactionId, source, target string, amount int64, currency string) {
		// Gunakan context background baru karena context gRPC asli akan dibatalkan saat fungsi return
		asyncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		javaAdapterURL := os.Getenv("JAVA_MQ_ADAPTER_URL")
		if javaAdapterURL == "" {
			javaAdapterURL = "http://localhost:8085/api/mq/publish" // Default local fallback
		}

		payload := map[string]interface{}{
			"transaction_id": transactionId,
			"source_account": source,
			"target_account": target,
			"amount":         amount,
			"currency":       currency,
			"timestamp":      time.Now().Format(time.RFC3339),
			"status":         "SUCCESS",
		}
		
		jsonData, err := json.Marshal(payload)
		if err != nil {
			slog.Error("Failed to marshal MQ payload", slog.String("error", err.Error()))
			return
		}

		req, err := http.NewRequestWithContext(asyncCtx, "POST", javaAdapterURL, bytes.NewBuffer(jsonData))
		if err != nil {
			slog.Error("Failed to create MQ HTTP request", slog.String("error", err.Error()))
			return
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("Failed to send log to Java MQ Adapter (ignored for local dev)", slog.String("error", err.Error()))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			slog.Info("Successfully pushed transaction log to Java MQ Adapter", slog.String("transaction_id", transactionId))
		} else {
			slog.Warn("Java MQ Adapter returned non-200 status", slog.Int("status_code", resp.StatusCode))
		}
	}(txnID, req.GetSourceAccount(), req.GetTargetAccount(), req.GetAmount(), req.GetCurrency())

	grpcRequestsTotal.WithLabelValues("transfer-service", method, "OK").Inc()
	grpcRequestDuration.WithLabelValues("transfer-service", method, "OK").Observe(time.Since(start).Seconds())
	log.InfoContext(ctx, "Fund transfer completed successfully", slog.String("transaction_id", txnID))

	return &pb.TransferResponse{
		TransactionId: txnID,
		Success:       true,
		Message:       "transfer successful",
	}, nil
}

func (s *server) recordTransfer(ctx context.Context, txnID string, req *pb.TransferRequest, status string) {
	_, err := s.db.ExecContext(ctx, 
		"INSERT INTO transfers (transaction_id, source_account, target_account, amount, currency, status) VALUES ($1, $2, $3, $4, $5, $6)",
		txnID, req.GetSourceAccount(), req.GetTargetAccount(), req.GetAmount(), req.GetCurrency(), status,
	)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to record transfer in database", slog.String("transaction_id", txnID), slog.String("error", err.Error()))
	}
}

type TransferPayload struct {
	SourceAccount string `json:"source_account"`
	TargetAccount string `json:"target_account"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
}

// REST HTTP Handler for Transfer
func (s *server) handleTransferREST(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Inject basic correlation ID
	corrID := r.Header.Get("X-Correlation-ID")
	if corrID != "" {
		ctx = context.WithValue(ctx, logger.CorrelationIDKey, corrID)
	}
	chID := r.Header.Get("X-Channel-ID")
	if chID != "" {
		ctx = context.WithValue(ctx, logger.ChannelIDKey, chID)
	}

	var payload TransferPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, `{"error":"Invalid request payload JSON"}`, http.StatusBadRequest)
		return
	}

	res, err := s.Transfer(ctx, &pb.TransferRequest{
		SourceAccount: payload.SourceAccount,
		TargetAccount: payload.TargetAccount,
		Amount:        payload.Amount,
		Currency:      payload.Currency,
	})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf(`{"error":"failed to execute transfer: %s"}`, err.Error())))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if res.GetSuccess() {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	json.NewEncoder(w).Encode(res)
}

func correlationInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	var correlationID string
	var channelID string
	if ok {
		if ids := md.Get("x-correlation-id"); len(ids) > 0 {
			correlationID = ids[0]
		}
		if ids := md.Get("x-channel-id"); len(ids) > 0 {
			channelID = ids[0]
		}
	}
	if correlationID != "" {
		ctx = context.WithValue(ctx, logger.CorrelationIDKey, correlationID)
	}
	if channelID != "" {
		ctx = context.WithValue(ctx, logger.ChannelIDKey, channelID)
	}
	return handler(ctx, req)
}

func initDb(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS transfers (
			id SERIAL PRIMARY KEY,
			transaction_id VARCHAR(100) UNIQUE NOT NULL,
			source_account VARCHAR(50) NOT NULL,
			target_account VARCHAR(50) NOT NULL,
			amount BIGINT NOT NULL,
			currency VARCHAR(3) NOT NULL,
			status VARCHAR(50) NOT NULL,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
		);

		-- Parameter Tables
		CREATE TABLE IF NOT EXISTS error_mappings (
			id SERIAL PRIMARY KEY,
			feature_name VARCHAR(50) NOT NULL,
			source_code VARCHAR(50) NOT NULL,
			target_code VARCHAR(50) NOT NULL,
			target_message VARCHAR(255) NOT NULL,
			UNIQUE(feature_name, source_code)
		);

		CREATE TABLE IF NOT EXISTS fees (
			id SERIAL PRIMARY KEY,
			feature_name VARCHAR(50) NOT NULL,
			customer_tier VARCHAR(50) NOT NULL,
			fee_amount BIGINT NOT NULL,
			currency VARCHAR(3) DEFAULT 'IDR',
			UNIQUE(feature_name, customer_tier)
		);

		CREATE TABLE IF NOT EXISTS gl_mappings (
			id SERIAL PRIMARY KEY,
			feature_name VARCHAR(50) NOT NULL,
			debit_gl_account VARCHAR(50) NOT NULL,
			credit_gl_account VARCHAR(50) NOT NULL,
			UNIQUE(feature_name)
		);

		-- Triggers for LISTEN/NOTIFY
		CREATE OR REPLACE FUNCTION notify_param_update() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_notify('param_updates', row_to_json(NEW)::text);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		DROP TRIGGER IF EXISTS error_mappings_notify ON error_mappings;
		CREATE TRIGGER error_mappings_notify AFTER INSERT OR UPDATE OR DELETE ON error_mappings FOR EACH ROW EXECUTE FUNCTION notify_param_update();

		DROP TRIGGER IF EXISTS fees_notify ON fees;
		CREATE TRIGGER fees_notify AFTER INSERT OR UPDATE OR DELETE ON fees FOR EACH ROW EXECUTE FUNCTION notify_param_update();

		DROP TRIGGER IF EXISTS gl_mappings_notify ON gl_mappings;
		CREATE TRIGGER gl_mappings_notify AFTER INSERT OR UPDATE OR DELETE ON gl_mappings FOR EACH ROW EXECUTE FUNCTION notify_param_update();
	`)
	return err
}

func main() {
	logFilePath := os.Getenv("LOG_FILE_PATH")
	log := logger.InitLogger("transfer-service", logFilePath)

	log.Info("Starting Transfer Service...")

	// Viper Configuration Setup
	viper.SetConfigName("application")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/banking/config")
	viper.AddConfigPath("../config")
	viper.AddConfigPath("./config")

	if err := viper.ReadInConfig(); err != nil {
		log.Warn("Failed to read viper config, using defaults", slog.String("error", err.Error()))
	} else {
		log.Info("Viper config loaded successfully", slog.Bool("maintenance_mode", viper.GetBool("app.maintenance_mode")))
	}

	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Info("Config file changed!", slog.String("name", e.Name))
		log.Info("Maintenance mode is now", slog.Bool("mode", viper.GetBool("app.maintenance_mode")))
	})
	viper.WatchConfig()

	// Database connection setup
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")

	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", dbHost, dbPort, dbUser, dbPassword, dbName)
	var db *sql.DB
	var err error

	// Retry database connection
	for i := 1; i <= 10; i++ {
		db, err = sql.Open("postgres", connStr)
		if err == nil {
			err = db.Ping()
			if err == nil {
				break
			}
		}
		log.Warn("Failed to connect to database, retrying in 2 seconds...", slog.Int("attempt", i), slog.String("error", err.Error()))
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Error("Database connection failed permanently", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	if err := initDb(db); err != nil {
		log.Error("Database initialization failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Initialize Parameter Cache and Postgres Listener
	InitCache(db, connStr)

	// Establish connection to Account Service (gRPC Client)
	acctAddr := os.Getenv("ACCOUNT_SERVICE_ADDR")
	var conn *grpc.ClientConn
	for i := 1; i <= 10; i++ {
		conn, err = grpc.NewClient(acctAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			break
		}
		log.Warn("Failed to connect to Account Service gRPC, retrying in 2 seconds...", slog.Int("attempt", i), slog.String("error", err.Error()))
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Error("Account Service client connection failed permanently", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer conn.Close()
	acctClient := pb.NewAccountServiceClient(conn)

	// Start Prometheus Metrics server in background
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Info("Starting HTTP Metrics Server on port 9002...")
		if err := http.ListenAndServe(":9002", nil); err != nil {
			log.Error("HTTP Metrics Server exited with error", slog.String("error", err.Error()))
		}
	}()

	// Establish connection to Vendor Adapter (gRPC Client)
	adapterAddr := os.Getenv("VENDOR_ADAPTER_ADDR")
	if adapterAddr == "" {
		adapterAddr = "localhost:8084"
	}
	var adapterConn *grpc.ClientConn
	for i := 1; i <= 10; i++ {
		adapterConn, err = grpc.NewClient(adapterAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			break
		}
		log.Warn("Failed to connect to Vendor Adapter gRPC, retrying...", slog.Int("attempt", i), slog.String("error", err.Error()))
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Error("Vendor Adapter client connection failed permanently", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer adapterConn.Close()
	adapterClient := adapterpb.NewVendorAdapterServiceClient(adapterConn)

	srv := &server{db: db, acctClient: acctClient, adapterClient: adapterClient}

	// Start REST Server in background
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/transfer", srv.handleTransferREST)
		mux.HandleFunc("/api/v1/internal/admin/reload-cache", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			ReloadAllCache()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"success","message":"Parameter cache reloaded successfully"}`))
		})
		
		log.Info("Starting REST HTTP server on port 8080...")
		if err := http.ListenAndServe(":8080", mux); err != nil {
			log.Error("REST HTTP server exited with error", slog.String("error", err.Error()))
		}
	}()

	// Start gRPC Server
	lis, err := net.Listen("tcp", ":8082")
	if err != nil {
		log.Error("Failed to listen on port 8082", slog.String("error", err.Error()))
		os.Exit(1)
	}

	s := grpc.NewServer(
		grpc.UnaryInterceptor(correlationInterceptor),
	)
	pb.RegisterTransferServiceServer(s, srv)

	go func() {
		log.Info("gRPC server listening on port 8082...")
		if err := s.Serve(lis); err != nil {
			log.Error("gRPC server exited with error", slog.String("error", err.Error()))
		}
	}()

	// Graceful shutdown handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Info("SIGTERM/SIGINT received: Commencing graceful shutdown of Transfer Service...")
	s.GracefulStop()
	logger.FlushSync()
	log.Info("Transfer Service successfully stopped.")
}
