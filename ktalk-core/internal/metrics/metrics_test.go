package metrics_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cwash797-cmd/Project-XK/ktalk-core/internal/metrics"
)

func TestHealthEndpoint(t *testing.T) {
	c := &metrics.Counters{}
	srv := metrics.New("0.1.0-test", c)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["version"] != "0.1.0-test" {
		t.Errorf("version = %v, want 0.1.0-test", body["version"])
	}
}

func TestMetricsEndpoint(t *testing.T) {
	c := &metrics.Counters{}
	c.BytesIn.Add(1024)
	c.BytesOut.Add(512)
	c.StreamsOpened.Add(5)
	c.StreamsClosed.Add(3)

	srv := metrics.New("0.1.0-test", c)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	checks := []string{
		"xk_bytes_in_total 1024",
		"xk_bytes_out_total 512",
		"xk_streams_opened_total 5",
		"xk_streams_closed_total 3",
		"xk_active_streams 2",
		"xk_uptime_seconds",
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\nbody:\n%s", want, body)
		}
	}
}
