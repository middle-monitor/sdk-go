package middlemonitor

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
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
	flusherMu        sync.Mutex
	flusherOnce      sync.Once
	flusherStop      chan struct{}
)

// startLogFlusher launches a single background goroutine that periodically flushes
// the log buffer, so low-volume logs are delivered within logFlushInterval.
func startLogFlusher() {
	flusherOnce.Do(func() {
		flusherMu.Lock()
		flusherStop = make(chan struct{})
		stop := flusherStop
		flusherMu.Unlock()

		// Read the interval here, not inside the goroutine: it keeps the only
		// access on the caller's side.
		interval := logFlushInterval

		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					FlushLogs(context.Background())
				case <-stop:
					return
				}
			}
		}()
	})
}

// stopLogFlusher ends the background goroutine. Without it a shut-down client
// leaves a ticker running for the rest of the process lifetime, flushing into a
// closed exporter.
func stopLogFlusher() {
	flusherMu.Lock()
	defer flusherMu.Unlock()
	if flusherStop != nil {
		close(flusherStop)
		flusherStop = nil
	}
	flusherOnce = sync.Once{}
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

	record := buildLogRecord(ctx, level, message, attrs, cfg)
	logBufferMu.Lock()
	logBuffer = append(logBuffer, record)
	shouldFlush := len(logBuffer) >= logBufferSize
	logBufferMu.Unlock()

	if shouldFlush {
		go flushLogs(ctx, client, cfg)
	}
}

// logHTTPRequest records one entry per instrumented HTTP request, so the Logs
// view carries per-service request volume — the half of the correlation that
// host CPU/memory metrics cannot provide on their own.
//
// Gated by the log sampling rules, which is what keeps this from becoming an
// access-log firehose: with the defaults only failed requests get through (4xx
// as WARN, 5xx as ERROR), never the 2xx traffic. Widen it with
// Sampling.Logs.Levels or AlwaysCaptureRoutes when a route deserves every hit.
func logHTTPRequest(ctx context.Context, cfg *Config, method, route string, status int, duration time.Duration, hasError bool, cause string) {
	if cfg == nil {
		return
	}
	level := httpStatusToLevel(status)
	if !cfg.ShouldSampleLog(route, level, status, hasError) {
		return
	}

	message := fmt.Sprintf("%s %s %d", method, route, status)
	// The cause is already extracted for the Errors view; carrying it here is
	// what makes a 5xx line readable without opening the trace.
	if cause != "" && !isGenericExceptionMessage(cause) {
		message += ": " + cause
	}

	Log(ctx, level, message, map[string]string{
		"http.method":      method,
		"http.route":       route,
		"http.status_code": strconv.Itoa(status),
		"duration_ms":      strconv.FormatInt(duration.Milliseconds(), 10),
	})
}

// httpStatusToLevel maps a response status onto the levels the sampling rules are
// written in: 5xx is the service failing, 4xx is the caller at fault.
func httpStatusToLevel(status int) LogLevel {
	switch {
	case status >= 500:
		return LogLevelERROR
	case status >= 400:
		return LogLevelWARN
	default:
		return LogLevelINFO
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
	record := buildLogRecord(ctx, level, message, attrs, cfg)
	return sendLogs(ctx, []*logspb.LogRecord{record}, cfg)
}

// FlushLogs sends any buffered logs immediately. Call before shutdown to avoid losing logs.
func FlushLogs(ctx context.Context) {
	flushLogs(ctx, GetGlobalClient(), GetGlobalConfig())
}

func buildLogRecord(ctx context.Context, level LogLevel, message string, attrs map[string]string, cfg *Config) *logspb.LogRecord {
	now := uint64(time.Now().UnixNano())
	severityNum := logLevelToSeverity(level)
	severityText := string(level)

	record := &logspb.LogRecord{
		TimeUnixNano:   now,
		SeverityNumber: severityNum,
		SeverityText:   severityText,
		Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: message}},
	}

	// Carry the active span so the backend can link the log to its trace.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		traceID := sc.TraceID()
		spanID := sc.SpanID()
		record.TraceId = traceID[:]
		record.SpanId = spanID[:]
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

	resAttrs := []*commonpb.KeyValue{
		{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: cfg.Service}}},
	}
	// Same host label as the trace/metric resource, so log volume and host
	// metrics can be lined up on the same host over the same window.
	if cfg.Hostname != "" {
		resAttrs = append(resAttrs, &commonpb.KeyValue{
			Key:   "host.name",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: cfg.Hostname}},
		})
	}

	resource := &resourcepb.Resource{Attributes: resAttrs}

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
