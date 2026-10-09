package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHealthcheckRequiresHealthyHTTPResponse(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusServiceUnavailable, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("healthcheck path = %q", r.URL.Path)
				}
				w.Header().Set("Location", "/healthz")
				w.WriteHeader(status)
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if err := healthcheck(target.Host); (err == nil) != (status == http.StatusNoContent) {
				t.Fatalf("healthcheck status %d: %v", status, err)
			}
		})
	}
}

func TestHealthcheckUsesConfiguredListener(t *testing.T) {
	for _, test := range []struct{ addr, want string }{
		{"", "http://127.0.0.1:8080/healthz"},
		{":9091", "http://127.0.0.1:9091/healthz"},
		{"0.0.0.0:9091", "http://127.0.0.1:9091/healthz"},
		{"[::]:9091", "http://[::1]:9091/healthz"},
		{"127.0.0.2:9091", "http://127.0.0.2:9091/healthz"},
	} {
		t.Run(test.addr, func(t *testing.T) {
			got, err := healthcheckURL(test.addr)
			if err != nil || got != test.want {
				t.Fatalf("healthcheckURL(%q) = %q, %v; want %q", test.addr, got, err, test.want)
			}
		})
	}
	if _, err := healthcheckURL("invalid"); err == nil {
		t.Fatal("invalid HTTP_ADDR accepted")
	}
}
