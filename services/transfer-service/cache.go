package main

import (
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Data Structures
type ErrorMapping struct {
	TargetCode    string
	TargetMessage string
}

type FeeMapping struct {
	FeeAmount int64
	Currency  string
}

type GLMapping struct {
	DebitGL  string
	CreditGL string
}

// In-Memory Caches
var (
	errorCache = make(map[string]map[string]ErrorMapping) // feature -> source_code -> ErrorMapping
	feeCache   = make(map[string]map[string]FeeMapping)   // feature -> tier -> FeeMapping
	glCache    = make(map[string]GLMapping)               // feature -> GLMapping

	cacheMutex sync.RWMutex
	globalDB   *sql.DB
)

// InitCache initializes the connection and loads all data
func InitCache(db *sql.DB, connStr string) {
	globalDB = db
	ReloadAllCache()

	// Start Postgres Listener in background
	go startListener(connStr)
}

// ReloadAllCache forces a full refresh from DB
func ReloadAllCache() {
	slog.Info("Reloading all parameter caches from PostgreSQL...")
	
	// Reload Error Mappings
	errMap, err := fetchErrorMappings(globalDB)
	if err != nil {
		slog.Error("Failed to fetch error mappings", slog.String("error", err.Error()))
	}
	
	// Reload Fees
	feeMap, err := fetchFees(globalDB)
	if err != nil {
		slog.Error("Failed to fetch fees", slog.String("error", err.Error()))
	}
	
	// Reload GL Mappings
	glMap, err := fetchGLMappings(globalDB)
	if err != nil {
		slog.Error("Failed to fetch GL mappings", slog.String("error", err.Error()))
	}

	// Safely swap memory
	cacheMutex.Lock()
	if errMap != nil {
		errorCache = errMap
	}
	if feeMap != nil {
		feeCache = feeMap
	}
	if glMap != nil {
		glCache = glMap
	}
	cacheMutex.Unlock()
	slog.Info("Parameter caches successfully reloaded")
}

func fetchErrorMappings(db *sql.DB) (map[string]map[string]ErrorMapping, error) {
	rows, err := db.Query("SELECT feature_name, source_code, target_code, target_message FROM error_mappings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make(map[string]map[string]ErrorMapping)
	for rows.Next() {
		var f, sc, tc, tm string
		if err := rows.Scan(&f, &sc, &tc, &tm); err != nil {
			continue
		}
		if _, ok := res[f]; !ok {
			res[f] = make(map[string]ErrorMapping)
		}
		res[f][sc] = ErrorMapping{TargetCode: tc, TargetMessage: tm}
	}
	return res, nil
}

func fetchFees(db *sql.DB) (map[string]map[string]FeeMapping, error) {
	rows, err := db.Query("SELECT feature_name, customer_tier, fee_amount, currency FROM fees")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make(map[string]map[string]FeeMapping)
	for rows.Next() {
		var f, tier, curr string
		var amount int64
		if err := rows.Scan(&f, &tier, &amount, &curr); err != nil {
			continue
		}
		if _, ok := res[f]; !ok {
			res[f] = make(map[string]FeeMapping)
		}
		res[f][tier] = FeeMapping{FeeAmount: amount, Currency: curr}
	}
	return res, nil
}

func fetchGLMappings(db *sql.DB) (map[string]GLMapping, error) {
	rows, err := db.Query("SELECT feature_name, debit_gl_account, credit_gl_account FROM gl_mappings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make(map[string]GLMapping)
	for rows.Next() {
		var f, debit, credit string
		if err := rows.Scan(&f, &debit, &credit); err != nil {
			continue
		}
		res[f] = GLMapping{DebitGL: debit, CreditGL: credit}
	}
	return res, nil
}

// Background Listener for Postgres NOTIFY
func startListener(connStr string) {
	reportProblem := func(ev pq.ListenerEventType, err error) {
		if err != nil {
			slog.Error("Postgres Listener error", slog.String("error", err.Error()))
		}
	}

	listener := pq.NewListener(connStr, 10*time.Second, time.Minute, reportProblem)
	defer listener.Close()

	err := listener.Listen("param_updates")
	if err != nil {
		slog.Error("Failed to start Postgres listener", slog.String("error", err.Error()))
		return
	}

	slog.Info("PostgreSQL VIP Listener started on channel 'param_updates'")

	for {
		select {
		case n := <-listener.Notify:
			if n == nil {
				continue
			}
			slog.Info("Received NOTIFY from Postgres", slog.String("channel", n.Channel), slog.String("payload", n.Extra))
			handleNotify(n.Extra)
		case <-time.After(90 * time.Second):
			// Ping listener to keep connection alive
			go listener.Ping()
		}
	}
}

// Payload format expected: {"table": "fees", "feature_name": "BI_FAST"}
type notifyPayload struct {
	Table       string `json:"table"`
	FeatureName string `json:"feature_name"`
}

func handleNotify(payloadStr string) {
	// For simplicity and since tables are very small, we trigger a full reload.
	// In extreme scale, we would do a targeted SELECT for just that feature.
	slog.Info("Reloading caches due to NOTIFY event", slog.String("payload", payloadStr))
	ReloadAllCache()
}

// Getters (Thread-Safe O(1) Lookup)
func GetErrorMapping(feature, sourceCode string) (ErrorMapping, bool) {
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()
	if featureMap, ok := errorCache[feature]; ok {
		if mapping, ok := featureMap[sourceCode]; ok {
			return mapping, true
		}
	}
	return ErrorMapping{}, false
}

func GetFee(feature, customerTier string) (FeeMapping, bool) {
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()
	if featureMap, ok := feeCache[feature]; ok {
		if mapping, ok := featureMap[customerTier]; ok {
			return mapping, true
		}
	}
	return FeeMapping{}, false
}

func GetGLMapping(feature string) (GLMapping, bool) {
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()
	mapping, ok := glCache[feature]
	return mapping, ok
}
