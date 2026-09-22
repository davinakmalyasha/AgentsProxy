package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireAdmin_OK(t *testing.T) {
	const key = "supersecret"
	h := RequireAdmin(key)
	called := false
	srv := httptest.NewServer(h(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if !IsAdmin(r.Context()) {
			t.Error("IsAdmin false on authed req")
		}
		w.WriteHeader(200)
	})))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !called || resp.StatusCode != 200 {
		t.Errorf("called=%v status=%d", called, resp.StatusCode)
	}
}

func TestRequireAdmin_Missing(t *testing.T) {
	h := RequireAdmin("k")
	srv := httptest.NewServer(h(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestRequireAdmin_Wrong(t *testing.T) {
	h := RequireAdmin("k")
	srv := httptest.NewServer(h(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestIsAdmin_Empty(t *testing.T) {
	if IsAdmin(context.Background()) {
		t.Error("IsAdmin true on bare ctx")
	}
}
