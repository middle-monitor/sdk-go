package middlemonitor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"runtime"
	"strings"
	"time"
)

const defaultPprofURL = "http://localhost:6060"

// CaptureCPUProfile collects a CPU profile for the given duration from the local pprof server,
// then uploads it to the Middle-Monitor API. The process must expose pprof (e.g. import _ "net/http/pprof" and serve on PprofURL).
func (c *Client) CaptureCPUProfile(ctx context.Context, duration time.Duration) error {
	cfg := c.config
	if cfg == nil || cfg.Endpoint == "" || cfg.Token == "" {
		return ErrConfigRequired
	}
	pprofURL := cfg.PprofURL
	if pprofURL == "" {
		pprofURL = defaultPprofURL
	}
	seconds := int(duration.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 120 {
		seconds = 120
	}
	url := strings.TrimSuffix(pprofURL, "/") + fmt.Sprintf("/debug/pprof/profile?seconds=%d", seconds)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create pprof request: %w", err)
	}
	client := &http.Client{Timeout: duration + 10*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch cpu profile: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pprof server returned %d: %s", resp.StatusCode, string(body))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read profile: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("empty profile data")
	}
	return uploadProfile(cfg, "cpu", &seconds, nil, data)
}

// CaptureHeapProfile collects a heap profile from the local pprof server and uploads it to the Middle-Monitor API.
func (c *Client) CaptureHeapProfile(ctx context.Context) error {
	cfg := c.config
	if cfg == nil || cfg.Endpoint == "" || cfg.Token == "" {
		return ErrConfigRequired
	}
	pprofURL := cfg.PprofURL
	if pprofURL == "" {
		pprofURL = defaultPprofURL
	}
	url := strings.TrimSuffix(pprofURL, "/") + "/debug/pprof/heap"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create pprof request: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch heap profile: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pprof server returned %d: %s", resp.StatusCode, string(body))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read profile: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("empty profile data")
	}
	var memoryMB *float64
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mb := float64(m.HeapInuse) / (1024 * 1024)
	memoryMB = &mb
	return uploadProfile(cfg, "heap", nil, memoryMB, data)
}

// apiBase returns the HTTP base URL for the Middle-Monitor API (with scheme).
func apiBase(cfg *Config) string {
	base := cfg.Endpoint
	if base != "" && !strings.Contains(cfg.Endpoint, "://") {
		if cfg.Insecure {
			base = "http://" + base
		} else {
			base = "https://" + base
		}
	}
	return strings.TrimSuffix(base, "/")
}

func uploadProfile(cfg *Config, profileType string, durationSeconds *int, memoryMB *float64, data []byte) error {
	base := apiBase(cfg)
	url := base + "/api/v1/profiles"
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	_ = w.WriteField("profile_type", profileType)
	_ = w.WriteField("service", cfg.Service)
	if durationSeconds != nil {
		_ = w.WriteField("duration_seconds", fmt.Sprintf("%d", *durationSeconds))
	}
	if memoryMB != nil {
		_ = w.WriteField("memory_mb", fmt.Sprintf("%.2f", *memoryMB))
	}
	part, err := w.CreateFormFile("profile", "profile.pprof")
	if err != nil {
		return fmt.Errorf("multipart create: %w", err)
	}
	if _, err = part.Write(data); err != nil {
		return fmt.Errorf("multipart write: %w", err)
	}
	contentType := w.FormDataContentType()
	if err = w.Close(); err != nil {
		return fmt.Errorf("multipart close: %w", err)
	}

	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload profile: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("upload returned %d: %s", resp.StatusCode, string(body))
	}
	log.Printf("[Middle-Monitor] uploaded %s profile (%d bytes)", profileType, len(data))
	return nil
}
