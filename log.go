package middlemonitor

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

var (
	logBuffer        []*logspb.LogRecord
	logBufferMu      sync.Mutex
	logBufferSize    = 10
	logFlushInterval = 5 * time.Second
	flusherOnce      sync.Once
)

// startLogFlusher launches a single background goroutine that periodically flushes
// the log buffer, so low-volume logs are delivered within logFlushInterval.
func startLogFlusher() {
	flusherOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(logFlushInterval)
			defer ticker.Stop()
			for range ticker.C {
				FlushLogs(context.Background())
			}
		}()
	})
}

// Log sends a log record to Middle-Monitor. It buffers logs and flushes periodically.
// level: DEBUG, INFO, WARN, ERROR, FATAL, PANIC
func Log(ctx context.Context, level LogLevel, message string, attrs map[string]string) {
	client := GetGlobalClient()
	if client == nil {
		return
	}
	cfg := GetGlobalConfig()
	if cfg == nil {
		return
	}

	// Ensure the periodic flusher is running
	startLogFlusher()

	record := buildLogRecord(level, message, attrs, cfg)
	logBufferMu.Lock()
	logBuffer = append(logBuffer, record)
	shouldFlush := len(logBuffer) >= logBufferSize
	logBufferMu.Unlock()

	if shouldFlush {
		go flushLogs(ctx, client, cfg)
	}
}

// LogSync immediately sends a log record to Middle-Monitor (no buffering).
func LogSync(ctx context.Context, level LogLevel, message string, attrs map[string]string) error {
	client := GetGlobalClient()
	if client == nil {
		return ErrNotInitialized
	}
	cfg := GetGlobalConfig()
	if cfg == nil {
		return ErrConfigMissing
	}
	record := buildLogRecord(level, message, attrs, cfg)
	return sendLogs(ctx, []*logspb.LogRecord{record}, cfg)
}

// FlushLogs sends any buffered logs immediately. Call before shutdown to avoid losing logs.
func FlushLogs(ctx context.Context) {
	flushLogs(ctx, GetGlobalClient(), GetGlobalConfig())
}

func buildLogRecord(level LogLevel, message string, attrs map[string]string, cfg *Config) *logspb.LogRecord {
	now := uint64(time.Now().UnixNano())
	severityNum := logLevelToSeverity(level)
	severityText := string(level)

	record := &logspb.LogRecord{
		TimeUnixNano:   now,
		SeverityNumber: severityNum,
		SeverityText:   severityText,
		Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: message}},
	}

	if len(attrs) > 0 {
		record.Attributes = make([]*commonpb.KeyValue, 0, len(attrs))
		for k, v := range attrs {
			record.Attributes = append(record.Attributes, &commonpb.KeyValue{
				Key:   k,
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}},
			})
		}
	}

	return record
}

func logLevelToSeverity(level LogLevel) logspb.SeverityNumber {
	switch level {
	case LogLevelDEBUG:
		return logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG
	case LogLevelINFO:
		return logspb.SeverityNumber_SEVERITY_NUMBER_INFO
	case LogLevelWARN:
		return logspb.SeverityNumber_SEVERITY_NUMBER_WARN
	case LogLevelERROR:
		return logspb.SeverityNumber_SEVERITY_NUMBER_ERROR
	case LogLevelFATAL, LogLevelPANIC:
		return logspb.SeverityNumber_SEVERITY_NUMBER_FATAL
	default:
		return logspb.SeverityNumber_SEVERITY_NUMBER_INFO
	}
}

func flushLogs(ctx context.Context, client *Client, cfg *Config) {
	if client == nil || cfg == nil {
		return
	}
	logBufferMu.Lock()
	if len(logBuffer) == 0 {
		logBufferMu.Unlock()
		return
	}
	toSend := logBuffer
	logBuffer = nil
	logBufferMu.Unlock()

	if err := sendLogs(ctx, toSend, cfg); err != nil {
		slog.Error("failed to flush logs", "error", err)
		// Re-add to buffer on failure
		logBufferMu.Lock()
		logBuffer = append(toSend, logBuffer...)
		logBufferMu.Unlock()
	}
}

func sendLogs(ctx context.Context, records []*logspb.LogRecord, cfg *Config) error {
	if len(records) == 0 {
		return nil
	}

	resource := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: cfg.Service}}},
		},
	}

	scopeLogs := &logspb.ScopeLogs{
		Scope: &commonpb.InstrumentationScope{
			Name:    "middle-monitor-sdk-go",
			Version: "1.0.0",
		},
		LogRecords: records,
	}

	resourceLogs := &logspb.ResourceLogs{
		Resource:  resource,
		ScopeLogs: []*logspb.ScopeLogs{scopeLogs},
	}

	req := &collectorlogs.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{resourceLogs},
	}

	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal logs: %w", ErrMarshal)
	}

	scheme := "https"
	if cfg.Insecure {
		scheme = "http"
	}
	url := fmt.Sprintf("%s://%s/v1/logs", scheme, cfg.Endpoint)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", ErrRequestCreate)
	}

	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	if cfg.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send logs: %w", ErrLogSend)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &HTTPStatusError{StatusCode: resp.StatusCode}
	}

	return nil
}
