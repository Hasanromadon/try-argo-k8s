package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"banking-microservices/pb"
	"banking-microservices/pkg/logger"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	_ "github.com/lib/pq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
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

	// URL Path parser regex for REST API
	balancePathRegex = regexp.MustCompile(`^/api/v1/accounts/([^/]+)/balance$`)
)

type server struct {
	pb.UnimplementedAccountServiceServer
	db *sql.DB
}

func (s *server) GetBalance(ctx context.Context, req *pb.GetBalanceRequest) (*pb.GetBalanceResponse, error) {
	start := time.Now()
	method := "GetBalance"
	log := slog.Default()

	log.InfoContext(ctx, "GetBalance request received", slog.String("account_number", req.GetAccountNumber()))

	var balance int64
	var currency string
	err := s.db.QueryRowContext(ctx, "SELECT balance, currency FROM accounts WHERE account_number = $1", req.GetAccountNumber()).Scan(&balance, &currency)
	if err != nil {
		if err == sql.ErrNoRows {
			grpcRequestsTotal.WithLabelValues("account-service", method, "NotFound").Inc()
			grpcRequestDuration.WithLabelValues("account-service", method, "NotFound").Observe(time.Since(start).Seconds())
			log.WarnContext(ctx, "Account not found", slog.String("account_number", req.GetAccountNumber()))
			return nil, status.Errorf(codes.NotFound, "account not found: %s", req.GetAccountNumber())
		}
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		grpcRequestDuration.WithLabelValues("account-service", method, "Internal").Observe(time.Since(start).Seconds())
		log.ErrorContext(ctx, "Failed to query account balance", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database error")
	}

	grpcRequestsTotal.WithLabelValues("account-service", method, "OK").Inc()
	grpcRequestDuration.WithLabelValues("account-service", method, "OK").Observe(time.Since(start).Seconds())
	log.InfoContext(ctx, "GetBalance request successful", slog.String("account_number", req.GetAccountNumber()), slog.Int64("balance", balance))

	return &pb.GetBalanceResponse{
		AccountNumber: req.GetAccountNumber(),
		Balance:       balance,
		Currency:      currency,
	}, nil
}

func (s *server) Debit(ctx context.Context, req *pb.DebitRequest) (*pb.DebitResponse, error) {
	start := time.Now()
	method := "Debit"
	log := slog.Default()

	log.InfoContext(ctx, "Debit request received", slog.String("account_number", req.GetAccountNumber()), slog.Int64("amount", req.GetAmount()))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		log.ErrorContext(ctx, "Failed to start database transaction", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database transaction error")
	}
	defer tx.Rollback()

	var balance int64
	err = tx.QueryRowContext(ctx, "SELECT balance FROM accounts WHERE account_number = $1 FOR UPDATE", req.GetAccountNumber()).Scan(&balance)
	if err != nil {
		if err == sql.ErrNoRows {
			grpcRequestsTotal.WithLabelValues("account-service", method, "NotFound").Inc()
			log.WarnContext(ctx, "Account not found for debit", slog.String("account_number", req.GetAccountNumber()))
			return &pb.DebitResponse{Success: false, Message: "account not found"}, nil
		}
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		log.ErrorContext(ctx, "Failed to lock account for debit", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database transaction error")
	}

	if balance < req.GetAmount() {
		grpcRequestsTotal.WithLabelValues("account-service", method, "FailedPrecondition").Inc()
		log.WarnContext(ctx, "Insufficient funds", slog.String("account_number", req.GetAccountNumber()), slog.Int64("balance", balance), slog.Int64("debit_amount", req.GetAmount()))
		return &pb.DebitResponse{Success: false, Message: "insufficient funds"}, nil
	}

	newBalance := balance - req.GetAmount()
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET balance = $1 WHERE account_number = $2", newBalance, req.GetAccountNumber())
	if err != nil {
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		log.ErrorContext(ctx, "Failed to update account balance for debit", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database update error")
	}

	err = tx.Commit()
	if err != nil {
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		log.ErrorContext(ctx, "Failed to commit debit transaction", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database commit error")
	}

	grpcRequestsTotal.WithLabelValues("account-service", method, "OK").Inc()
	grpcRequestDuration.WithLabelValues("account-service", method, "OK").Observe(time.Since(start).Seconds())
	log.InfoContext(ctx, "Debit transaction successful", slog.String("account_number", req.GetAccountNumber()), slog.Int64("new_balance", newBalance))

	return &pb.DebitResponse{
		AccountNumber: req.GetAccountNumber(),
		NewBalance:    newBalance,
		Success:       true,
		Message:       "debit successful",
	}, nil
}

func (s *server) Credit(ctx context.Context, req *pb.CreditRequest) (*pb.CreditResponse, error) {
	start := time.Now()
	method := "Credit"
	log := slog.Default()

	log.InfoContext(ctx, "Credit request received", slog.String("account_number", req.GetAccountNumber()), slog.Int64("amount", req.GetAmount()))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		log.ErrorContext(ctx, "Failed to start database transaction", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database transaction error")
	}
	defer tx.Rollback()

	var balance int64
	err = tx.QueryRowContext(ctx, "SELECT balance FROM accounts WHERE account_number = $1 FOR UPDATE", req.GetAccountNumber()).Scan(&balance)
	if err != nil {
		if err == sql.ErrNoRows {
			grpcRequestsTotal.WithLabelValues("account-service", method, "NotFound").Inc()
			log.WarnContext(ctx, "Account not found for credit", slog.String("account_number", req.GetAccountNumber()))
			return &pb.CreditResponse{Success: false, Message: "account not found"}, nil
		}
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		log.ErrorContext(ctx, "Failed to lock account for credit", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database transaction error")
	}

	newBalance := balance + req.GetAmount()
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET balance = $1 WHERE account_number = $2", newBalance, req.GetAccountNumber())
	if err != nil {
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		log.ErrorContext(ctx, "Failed to update account balance for credit", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database update error")
	}

	err = tx.Commit()
	if err != nil {
		grpcRequestsTotal.WithLabelValues("account-service", method, "Internal").Inc()
		log.ErrorContext(ctx, "Failed to commit credit transaction", slog.String("error", err.Error()))
		return nil, status.Errorf(codes.Internal, "database commit error")
	}

	grpcRequestsTotal.WithLabelValues("account-service", method, "OK").Inc()
	grpcRequestDuration.WithLabelValues("account-service", method, "OK").Observe(time.Since(start).Seconds())
	log.InfoContext(ctx, "Credit transaction successful", slog.String("account_number", req.GetAccountNumber()), slog.Int64("new_balance", newBalance))

	return &pb.CreditResponse{
		AccountNumber: req.GetAccountNumber(),
		NewBalance:    newBalance,
		Success:       true,
		Message:       "credit successful",
	}, nil
}

// REST HTTP Handler for GetBalance
func (s *server) handleBalanceREST(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	// Inject basic correlation ID
	corrID := r.Header.Get("X-Correlation-ID")
	if corrID != "" {
		ctx = context.WithValue(ctx, logger.CorrelationIDKey, corrID)
	}

	matches := balancePathRegex.FindStringSubmatch(r.URL.Path)
	if len(matches) < 2 {
		http.Error(w, `{"error":"invalid account number path"}`, http.StatusBadRequest)
		return
	}
	acctNum := matches[1]

	res, err := s.GetBalance(ctx, &pb.GetBalanceRequest{AccountNumber: acctNum})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		if status.Code(err) == codes.NotFound {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(fmt.Sprintf(`{"error":"%s"}`, err.Error())))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf(`{"error":"failed to fetch account details: %s"}`, err.Error())))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
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
		CREATE TABLE IF NOT EXISTS accounts (
			account_number VARCHAR(50) PRIMARY KEY,
			balance BIGINT NOT NULL,
			currency VARCHAR(3) NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("failed to create accounts table: %v", err)
	}

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check accounts count: %v", err)
	}

	if count == 0 {
		for i := 1; i <= 5; i++ {
			accNum := fmt.Sprintf("110-000-%d", i)
			_, err = db.Exec("INSERT INTO accounts (account_number, balance, currency) VALUES ($1, $2, $3)", accNum, 50000000, "IDR")
			if err != nil {
				return fmt.Errorf("failed to insert sample account %s: %v", accNum, err)
			}
		}
		slog.Info("Successfully populated sample database accounts")
	}

	return nil
}

func main() {
	logFilePath := os.Getenv("LOG_FILE_PATH")
	log := logger.InitLogger("account-service", logFilePath)

	log.Info("Starting Account Service...")

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

	// Start Prometheus Metrics server in background
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Info("Starting HTTP Metrics Server on port 9001...")
		if err := http.ListenAndServe(":9001", nil); err != nil {
			log.Error("HTTP Metrics Server exited with error", slog.String("error", err.Error()))
		}
	}()

	srv := &server{db: db}

	// Start REST Server in background
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/accounts/", srv.handleBalanceREST)
		log.Info("Starting REST HTTP server on port 8080...")
		if err := http.ListenAndServe(":8080", mux); err != nil {
			log.Error("REST HTTP server exited with error", slog.String("error", err.Error()))
		}
	}()

	// Start gRPC Server
	lis, err := net.Listen("tcp", ":8083")
	if err != nil {
		log.Error("Failed to listen on port 8083", slog.String("error", err.Error()))
		os.Exit(1)
	}

	s := grpc.NewServer(
		grpc.UnaryInterceptor(correlationInterceptor),
	)
	pb.RegisterAccountServiceServer(s, srv)

	go func() {
		log.Info("gRPC server listening on port 8083...")
		if err := s.Serve(lis); err != nil {
			log.Error("gRPC server exited with error", slog.String("error", err.Error()))
		}
	}()

	// Graceful shutdown handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Info("SIGTERM/SIGINT received: Commencing graceful shutdown of Account Service...")
	s.GracefulStop()
	logger.FlushSync()
	log.Info("Account Service successfully stopped.")
}
