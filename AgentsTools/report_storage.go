package agentstools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/httpx"
)

// ReportStore persists chat-generated report bytes. Local disk is the dev
// default; Supabase Storage is the durable production backend. The ticket
// layer above never knows which one is wired: it stores an opaque object
// key and loads bytes back by key.
type ReportStore interface {
	// Save persists data under name and returns the opaque object key the
	// ticket must reference.
	Save(ctx context.Context, tenant, name string, data []byte) (objectKey string, err error)
	// Load returns the download filename + bytes for an object key.
	Load(ctx context.Context, tenant, objectKey string) (filename string, data []byte, err error)
	// Delete removes an object key idempotently.
	Delete(ctx context.Context, tenant, objectKey string) error
}

// ReportStorage is the active backend. Local disk unless main wires
// Supabase from env.
var ReportStorage ReportStore = LocalReportStore{}

// ReportMaxBytes caps a generated workbook before upload. Reports are
// bursty and low-volume; the cap is defense-in-depth, not a quota.
var ReportMaxBytes = 15_000_000

// ConfigureSupabaseReportStore switches the backend to Supabase Storage.
// Empty url or key keeps local disk (dev/tests).
func ConfigureSupabaseReportStore(baseURL, apiKey, bucket string) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.TrimSpace(apiKey) == "" {
		return
	}
	if strings.TrimSpace(bucket) == "" {
		bucket = "reports"
	}
	ReportStorage = &SupabaseReportStore{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Bucket:  bucket,
		HTTP:    httpx.NewClient(30 * time.Second),
	}
}

// LocalReportStore is the dev/test backend: data/reports/{tenant}/{file}.
type LocalReportStore struct{}

// IsLocal reports whether this backend is local disk (legacy sweep path).
func (LocalReportStore) IsLocal() bool { return true }

func (LocalReportStore) Save(ctx context.Context, tenant, name string, data []byte) (string, error) {
	t := SafeTenant(tenant)
	base := filepath.Base(name)
	if base == "" || base != name {
		return "", fmt.Errorf("bad report name")
	}
	dir := filepath.Join(ReportsDir, t)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, base), data, 0o640); err != nil {
		return "", err
	}
	return base, nil
}

func (LocalReportStore) Load(ctx context.Context, tenant, objectKey string) (string, []byte, error) {
	t := SafeTenant(tenant)
	base := filepath.Base(objectKey)
	if base == "" || base != objectKey {
		return "", nil, fmt.Errorf("bad ticket")
	}
	b, err := os.ReadFile(filepath.Join(ReportsDir, t, base))
	if err != nil {
		return "", nil, fmt.Errorf("file gone")
	}
	return base, b, nil
}

func (LocalReportStore) Delete(ctx context.Context, tenant, objectKey string) error {
	t := SafeTenant(tenant)
	base := filepath.Base(objectKey)
	if base == "" || base != objectKey {
		return nil
	}
	err := os.Remove(filepath.Join(ReportsDir, t, base))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SupabaseReportStore is the durable production backend: one private
// `reports` bucket, tenant-prefixed object keys, app-proxied downloads.
// Auth stays the Redis ticket checked in Go; Storage never sees end users.
type SupabaseReportStore struct {
	BaseURL string
	APIKey  string // service-scoped key, server-only
	Bucket  string
	HTTP    *http.Client
}

func (s *SupabaseReportStore) http() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	// Shared transport, not a fresh client per call (which also
	// skipped connection reuse entirely).
	return httpx.NewClient(30 * time.Second)
}

// objectKey builds the server-side key. Never client-supplied: tenant is
// SafeTenant-shaped, name is the server-generated filename.
func (s *SupabaseReportStore) objectKey(tenant, name string) (string, error) {
	t := SafeTenant(tenant)
	base := filepath.Base(name)
	if base == "" || base != name || !strings.HasSuffix(base, ".xlsx") {
		return "", fmt.Errorf("bad report name")
	}
	month := time.Now().In(WibLoc()).Format("2006-01")
	key := "reports/" + t + "/" + month + "/" + base
	return key, nil
}

// escapeKey escapes each path segment, keeping slashes as separators.
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (s *SupabaseReportStore) req(ctx context.Context, method, path string, body []byte, contentType string) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, s.BaseURL+"/storage/v1/"+strings.TrimPrefix(path, "/"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("apikey", s.APIKey)
	r.Header.Set("Authorization", "Bearer "+s.APIKey)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r, nil
}

func (s *SupabaseReportStore) Save(ctx context.Context, tenant, name string, data []byte) (string, error) {
	key, err := s.objectKey(tenant, name)
	if err != nil {
		return "", err
	}
	r, err := s.req(ctx, http.MethodPost, "object/"+s.Bucket+"/"+escapeKey(key), data,
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	if err != nil {
		return "", err
	}
	r.Header.Set("x-upsert", "true")
	resp, err := s.http().Do(r)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("storage upload: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return key, nil
}

func (s *SupabaseReportStore) Load(ctx context.Context, tenant, objectKey string) (string, []byte, error) {
	if !strings.HasPrefix(objectKey, "reports/") {
		return "", nil, fmt.Errorf("bad ticket")
	}
	r, err := s.req(ctx, http.MethodGet, "object/authenticated/"+s.Bucket+"/"+escapeKey(objectKey), nil, "")
	if err != nil {
		return "", nil, err
	}
	resp, err := s.http().Do(r)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("file gone")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(ReportMaxBytes)+1))
	if err != nil {
		return "", nil, err
	}
	parts := strings.Split(objectKey, "/")
	return parts[len(parts)-1], b, nil
}

func (s *SupabaseReportStore) Delete(ctx context.Context, tenant, objectKey string) error {
	if !strings.HasPrefix(objectKey, "reports/") {
		return nil
	}
	r, err := s.req(ctx, http.MethodDelete, "object/"+s.Bucket+"/"+escapeKey(objectKey), nil, "")
	if err != nil {
		return err
	}
	resp, err := s.http().Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("storage delete: status %d", resp.StatusCode)
	}
	return nil
}
