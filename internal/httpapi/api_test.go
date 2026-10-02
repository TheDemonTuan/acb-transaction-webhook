package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestAdminLifecycle(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store).Handler()
	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post("/api/v1/connection/configure", `{"accountMasked":"***1234"}`); w.Code != http.StatusCreated {
		t.Fatalf("configure %d %s", w.Code, w.Body.String())
	}
	if w := post("/api/v1/webhooks", `{"name":"receiver","url":"https://events.example.com/bank"}`); w.Code != http.StatusCreated {
		t.Fatalf("endpoint %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/webhooks", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get endpoints %d", w.Code)
	}
}

func TestGatewayDoesNotServeFrontendRoutes(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway-no-spa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	h := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store).Handler()
	for _, requestPath := range []string{"/", "/admin/activity", "/transactions/example"} {
		t.Run(requestPath, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://example.test"+requestPath, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("gateway must not serve frontend route %s; got %d", requestPath, w.Code)
			}
		})
	}
}
