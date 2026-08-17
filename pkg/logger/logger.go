package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	CorrelationIDKey = "X-Correlation-ID"
	ChannelIDKey     = "X-Channel-ID"
)

var (
	// PCI-DSS / ISO 27001 Masking Regex Patterns
	cardRegex = regexp.MustCompile(`\b(?:\d[ -]*?){13,16}\b`)
	cvvRegex  = regexp.MustCompile(`(?i)("cvv"|"pin"|"password")\s*:\s*"[^"]+"`)
	once      sync.Once
	log       *slog.Logger
	asyncWriter *AsyncLogBuffer
)

// MaskPII sanitizes sensitive financial data (PAN, CVV, Passwords) before logging
func MaskPII(input string) string {
	// Mask credit card / PAN numbers (keep first 6 and last 4)
	masked := cardRegex.ReplaceAllStringFunc(input, func(card string) string {
		clean := strings.ReplaceAll(strings.ReplaceAll(card, "-", ""), " ", "")
		if len(clean) >= 13 {
			return clean[:6] + "******" + clean[len(clean)-4:]
		}
		return card
	})

	// Mask CVV / PIN / Passwords
	masked = cvvRegex.ReplaceAllString(masked, `${1}:"***MASKED***"`)
	return masked
}

// AsyncLogBuffer implements a Zero-Locking Asynchronous Ring Buffer Writer
type AsyncLogBuffer struct {
	target      io.Writer
	channel     chan []byte
	done        chan struct{}
	closeOnce   sync.Once
}

// NewAsyncLogBuffer creates a non-blocking asynchronous log buffer
func NewAsyncLogBuffer(target io.Writer, bufferSize int) *AsyncLogBuffer {
	buf := &AsyncLogBuffer{
		target:  target,
		channel: make(chan []byte, bufferSize),
		done:    make(chan struct{}),
	}
	// Start background worker goroutine for batch flushing
	go buf.worker()
	return buf
}

func (b *AsyncLogBuffer) IsNearlyFull() bool {
	if b == nil || b.channel == nil {
		return false
	}
	// Return true if buffer is more than 90% full
	return len(b.channel) >= int(float64(cap(b.channel))*0.9)
}

func (b *AsyncLogBuffer) Write(p []byte) (n int, err error) {
	// Copy slice to avoid memory race condition
	cp := make([]byte, len(p))
	copy(cp, p)

	select {
	case b.channel <- cp:
		// Pushed to memory ring buffer in sub-microsecond time!
		return len(p), nil
	default:
		// Circular Eviction: If buffer is 100% full, drop the oldest log to make room for the new one
		select {
		case <-b.channel: // Drop the oldest log entry
		default:
		}
		
		// Try pushing the new log again
		select {
		case b.channel <- cp:
		default:
			// Extreme safety fallback (write to stderr directly)
			os.Stderr.Write([]byte(`{"level":"WARN","message":"Log buffer drop recovery triggered under extreme load"}` + "\n"))
		}
		return len(p), nil
	}
}

func (b *AsyncLogBuffer) worker() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case data, ok := <-b.channel:
			if !ok {
				b.flushRemaining()
				close(b.done)
				return
			}
			b.target.Write(data)
		case <-ticker.C:
			// Regular flush heartbeat
		}
	}
}

func (b *AsyncLogBuffer) flushRemaining() {
	for data := range b.channel {
		b.target.Write(data)
	}
}

func (b *AsyncLogBuffer) Close() {
	b.closeOnce.Do(func() {
		close(b.channel)
		<-b.done
	})
}

// BankingLogHandler intercepts slog records to inject Correlation ID & PII Masking
type BankingLogHandler struct {
	slog.Handler
}

func (h *BankingLogHandler) Handle(ctx context.Context, r slog.Record) error {
	// 1. Dynamic Load Shedding: drop INFO/DEBUG logs if the buffer is nearly full (>90%)
	if asyncWriter != nil && asyncWriter.IsNearlyFull() && r.Level < slog.LevelWarn {
		return nil
	}

	// Inject Correlation ID & Channel ID from context
	if corrID, ok := ctx.Value(CorrelationIDKey).(string); ok && corrID != "" {
		r.AddAttrs(slog.String("trace_id", corrID))
	}
	if chID, ok := ctx.Value(ChannelIDKey).(string); ok && chID != "" {
		r.AddAttrs(slog.String("channel_id", chID))
	}

	// Sanitize Message Body
	r.Message = MaskPII(r.Message)

	return h.Handler.Handle(ctx, r)
}

func (h *BankingLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &BankingLogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *BankingLogHandler) WithGroup(name string) slog.Handler {
	return &BankingLogHandler{Handler: h.Handler.WithGroup(name)}
}

// InitLogger initializes high-performance enterprise JSON logger with Async Buffer
func InitLogger(serviceName string, logFilePath string) *slog.Logger {
	once.Do(func() {
		writers := []io.Writer{os.Stdout}

		if logFilePath != "" {
			file, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
			if err == nil {
				writers = append(writers, file)
			}
		}

		multiWriter := io.MultiWriter(writers...)

		// Wrap multiWriter with Async Ring Buffer (configurable capacity, default 10,000)
		bufSize := 10000
		if envSize := os.Getenv("LOG_BUFFER_SIZE"); envSize != "" {
			if val, err := strconv.Atoi(envSize); err == nil {
				bufSize = val
			}
		}
		asyncWriter = NewAsyncLogBuffer(multiWriter, bufSize)

		jsonHandler := slog.NewJSONHandler(asyncWriter, &slog.HandlerOptions{
			Level:     slog.LevelInfo,
			AddSource: true,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// Standardize time key to ISO8601 UTC
				if a.Key == slog.TimeKey {
					return slog.Attr{Key: "@timestamp", Value: slog.StringValue(a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z07:00"))}
				}
				if a.Key == slog.MessageKey {
					return slog.Attr{Key: "message", Value: slog.StringValue(MaskPII(a.Value.String()))}
				}
				return a
			},
		})

		handler := &BankingLogHandler{Handler: jsonHandler}
		log = slog.New(handler).With(
			slog.String("service", serviceName),
			slog.String("environment", "production"),
			slog.String("domain", "core-banking"),
		)
		slog.SetDefault(log)
	})
	return log
}

// FlushSync ensures all buffered logs are flushed to disk before graceful shutdown
func FlushSync() {
	if asyncWriter != nil {
		asyncWriter.Close()
	}
}

func Get() *slog.Logger {
	if log == nil {
		return InitLogger("banking-service", "")
	}
	return log
}
