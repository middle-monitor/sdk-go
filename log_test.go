package middlemonitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// newLogTestConfig returns a Config pointing at the given test server.
func newLogTestConfig(serverURL string) *Config {
	cfg := NewConfig(serverURL, "test-svc", "tok")
	return cfg
}

// ── buildLogRecord ────────────────────────────────────────────────────────────

func TestBuildLogRecord_AllLevels(t *testing.T) {
	cfg := newTestConfig()
	levels := []LogLevel{
		LogLevelDEBUG, LogLevelINFO, LogLevelWARN,
		LogLevelERROR, LogLevelFATAL, LogLevelPANIC,
	}
	for _, lvl := range levels {
		rec := buildLogRecord(context.Background(), lvl, "test message", map[string]string{"k": "v"}, cfg)
		if rec == nil {
			t.Errorf("level %s: got nil record", lvl)
		}
		if rec.SeverityText != string(lvl) {
			t.Errorf("level %s: want SeverityText=%s, got %s", lvl, lvl, rec.SeverityText)
		}
		if rec.Body == nil {
			t.Errorf("level %s: body should not be nil", lvl)
		}
	}
}

func TestBuildLogRecord_NoAttrs(t *testing.T) {
	cfg := newTestConfig()
	rec := buildLogRecord(context.Background(), LogLevelINFO, "msg", nil, cfg)
	if rec == nil {
		t.Fatal("expected non-nil record")
	}
	if len(rec.Attributes) != 0 {
		t.Errorf("want 0 attrs, got %d", len(rec.Attributes))
	}
}

// ── logLevelToSeverity ────────────────────────────────────────────────────────

func TestLogLevelToSeverity_AllLevels(t *testing.T) {
	cases := []struct {
		level    LogLevel
		wantNot  logspb.SeverityNumber
		wantZero bool
	}{
		{LogLevelDEBUG, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelINFO, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelWARN, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelERROR, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelFATAL, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelPANIC, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
	}
	for _, tc := range cases {
		sev := logLevelToSeverity(tc.level)
		if sev == logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED {
			t.Errorf("level %s: unexpected UNSPECIFIED severity", tc.level)
		}
	}
}

func TestLogLevelToSeverity_Default(t *testing.T) {
	sev := logLevelToSeverity(LogLevel("UNKNOWN"))
	if sev != logspb.SeverityNumber_SEVERITY_NUMBER_INFO {
		t.Errorf("want INFO for unknown level, got %v", sev)
	}
}

// ── sendLogs ─────────────────────────────────────────────────────────────────

func TestSendLogs_Empty(t *testing.T) {
	cfg := newTestConfig()
	err := sendLogs(context.Background(), nil, cfg)
	if err != nil {
		t.Errorf("empty records should be no-op, got: %v", err)
	}
}

func TestSendLogs_Success(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := newLogTestConfig(srv.URL)
	cfg.Insecure = true
	rec := buildLogRecord(context.Background(), LogLevelERROR, "test error", nil, cfg)

	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Error("test server did not receive the request")
	}
}

func TestSendLogs_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := newLogTestConfig(srv.URL)
	cfg.Insecure = true
	rec := buildLogRecord(context.Background(), LogLevelERROR, "err", nil, cfg)

	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for non-200 status")
	}
}

func TestSendLogs_BadURL(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "not-a-host:99999"
	cfg.Insecure = true
	rec := buildLogRecord(context.Background(), LogLevelERROR, "err", nil, cfg)
	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for unreachable host")
	}
}

// ── flushLogs ─────────────────────────────────────────────────────────────────

func TestFlushLogs_NilClient(t *testing.T) {
	cfg := newTestConfig()
	// Should not panic
	flushLogs(context.Background(), nil, cfg)
}

func TestFlushLogs_NilConfig(t *testing.T) {
	// Should not panic
	flushLogs(context.Background(), &Client{}, nil)
}

func TestFlushLogs_EmptyBuffer(t *testing.T) {
	cfg := newTestConfig()
	client := &Client{}
	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()
	// Should not panic and should be a no-op
	flushLogs(context.Background(), client, cfg)
}

func TestFlushLogs_WithRecords_ServerDown(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "localhost:19999" // nothing listening
	cfg.Insecure = true
	client := &Client{}

	logBufferMu.Lock()
	logBuffer = []*logspb.LogRecord{
		buildLogRecord(context.Background(), LogLevelERROR, "err", nil, cfg),
	}
	logBufferMu.Unlock()

	// Should not panic — records are re-added on failure
	flushLogs(context.Background(), client, cfg)

	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()
}

// ── startLogFlusher ───────────────────────────────────────────────────────────

func TestStartLogFlusher_Idempotent(t *testing.T) {
	// Reset the flusherOnce so this test can exercise the code path
	flusherOnce = sync.Once{}
	startLogFlusher()
	// Calling again should be safe (idempotent)
	startLogFlusher()
}

// ── Log / LogSync / FlushLogs (global API, nil client path) ──────────────────

func TestLog_NoClient(t *testing.T) {
	resetGlobalState()
	// Should not panic when no global client is set
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

func TestLogSync_NoClient(t *testing.T) {
	resetGlobalState()
	// No token: auto-init stays off and GetGlobalClient() returns nil
	t.Setenv("MIDDLE_MONITOR_TOKEN", "")
	defer resetGlobalState()
	err := LogSync(context.Background(), LogLevelINFO, "msg", nil)
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestLogSync_NoConfig(t *testing.T) {
	resetGlobalState()
	// No token, then a client with no config so GetGlobalConfig() returns nil
	t.Setenv("MIDDLE_MONITOR_TOKEN", "")
	globalClient = &Client{}
	defer resetGlobalState()
	err := LogSync(context.Background(), LogLevelINFO, "msg", nil)
	if err != ErrConfigMissing {
		t.Errorf("want ErrConfigMissing, got %v", err)
	}
}

func TestFlushLogs_GlobalAPI(t *testing.T) {
	resetGlobalState()
	// Should not panic
	FlushLogs(context.Background())
}

// TestStartLogFlusher_GoroutineBody covers log.go:35-37 — the goroutine body that
// fires on each ticker tick. We shorten the interval so the test is fast.
// Shutdown must end the flusher goroutine. Left running, it keeps ticking for
// the rest of the process lifetime and flushes into providers already torn down
// — a leak in every long-lived process that restarts its client.
func TestShutdown_StopsLogFlusher(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	var flushes int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&flushes, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	old := logFlushInterval
	logFlushInterval = 20 * time.Millisecond
	defer func() { logFlushInterval = old }()

	if err := Init(newLogTestConfig(srv.URL)); err != nil {
		t.Fatalf("init: %v", err)
	}
	startLogFlusher()

	// A buffered record gives the ticker something to send.
	Log(context.Background(), LogLevelERROR, "before shutdown", nil)
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt64(&flushes) == 0 {
		t.Fatal("expected the flusher to have sent the buffered log")
	}

	client := GetGlobalClient()
	if client == nil {
		t.Fatal("expected an initialized client")
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Buffer another record: a stopped flusher must never pick it up. Appended
	// directly, because Log() restarts the flusher on purpose.
	settled := atomic.LoadInt64(&flushes)
	logBufferMu.Lock()
	logBuffer = append(logBuffer, buildLogRecord(context.Background(), LogLevelERROR, "after shutdown", nil, GetGlobalConfig()))
	logBufferMu.Unlock()

	time.Sleep(150 * time.Millisecond)
	if n := atomic.LoadInt64(&flushes); n != settled {
		t.Errorf("flusher still ticking after shutdown: %d flushes, want %d", n, settled)
	}
}

func TestStartLogFlusher_GoroutineBody(t *testing.T) {
	stopLogFlusher()
	old := logFlushInterval
	logFlushInterval = 20 * time.Millisecond
	defer func() {
		stopLogFlusher()
		logFlushInterval = old
	}()
	startLogFlusher()
	time.Sleep(80 * time.Millisecond)
}

// TestLog_NilClient_Blocked covers log.go:46-48 — early return when client is nil.
func TestLog_NilClient_Blocked(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	t.Setenv("MIDDLE_MONITOR_TOKEN", "") // auto-init stays off, client stays nil
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

// TestLog_NilConfig_Blocked covers log.go:50-52 — early return when config is nil.
func TestLog_NilConfig_Blocked(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	t.Setenv("MIDDLE_MONITOR_TOKEN", "") // auto-init stays off
	globalClient = &Client{}
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

// TestLog_ShouldFlush covers log.go:64-66 — the flush goroutine trigger when the
// buffer reaches logBufferSize entries.
func TestLog_ShouldFlush(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	cfg.Insecure = true
	Init(cfg)

	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()

	for i := 0; i < logBufferSize+1; i++ {
		Log(context.Background(), LogLevelINFO, "fill", nil)
	}
	time.Sleep(100 * time.Millisecond)
}

// TestLogSync_Success covers log.go:79-80 — the happy path of LogSync when both
// client and config are initialized.
func TestLogSync_Success(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	cfg.Insecure = true
	if err := Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}

	err := LogSync(context.Background(), LogLevelINFO, "hello", nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSendLogs_BadURL_InvalidHost covers log.go:193-195 — http.NewRequestWithContext
// error when the endpoint produces an invalid URL.
func TestSendLogs_BadURL_InvalidHost(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "[invalid"
	cfg.Insecure = true
	rec := buildLogRecord(context.Background(), LogLevelERROR, "err", nil, cfg)
	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for invalid endpoint URL")
	}
}

// A log carrying its span lets the backend join it to the trace timeline; without
// the ids the record is orphaned in OpenSearch, which is what makes correlated
// incident debugging work at all.
func TestBuildLogRecord_CarriesActiveSpan(t *testing.T) {
	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}

	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	rec := buildLogRecord(ctx, LogLevelERROR, "boom", nil, newTestConfig())

	if string(rec.TraceId) != string(traceID[:]) {
		t.Errorf("trace id not propagated: got %x, want %x", rec.TraceId, traceID[:])
	}
	if string(rec.SpanId) != string(spanID[:]) {
		t.Errorf("span id not propagated: got %x, want %x", rec.SpanId, spanID[:])
	}
}

// Logs emitted outside any span must stay unlinked rather than carry zeroed ids,
// which the backend would index as a real trace and never match.
func TestBuildLogRecord_NoSpanLeavesIdsEmpty(t *testing.T) {
	rec := buildLogRecord(context.Background(), LogLevelINFO, "no span", nil, newTestConfig())

	if len(rec.TraceId) != 0 {
		t.Errorf("expected no trace id, got %x", rec.TraceId)
	}
	if len(rec.SpanId) != 0 {
		t.Errorf("expected no span id, got %x", rec.SpanId)
	}
}
