package diag

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHitRate(t *testing.T) {
	cases := []struct {
		hits, misses uint64
		want         string
	}{
		{0, 0, "n/a"},
		{1, 0, "100%"},
		{0, 1, "0%"},
		{3, 1, "75%"},
		{1, 2, "33%"},
	}
	for _, c := range cases {
		if got := hitRate(c.hits, c.misses); got != c.want {
			t.Errorf("hitRate(%d,%d) = %q, want %q", c.hits, c.misses, got, c.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                         "0s",
		45 * time.Second:          "45s",
		90 * time.Second:          "1m30s",
		time.Hour + 5*time.Minute: "1h5m",
		2*time.Hour + 30*time.Minute + 1*time.Second: "2h30m",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestApiReach(t *testing.T) {
	// Any HTTP response (even 404) proves reachability.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ok.Close()
	if c := apiReach(ok.URL); c.Level != levelPass {
		t.Errorf("404 should still be reachable: got %s %s", c.Level, c.Detail)
	}

	// A 5xx is a server-side problem => WARN.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	if c := apiReach(bad.URL); c.Level != levelWarn {
		t.Errorf("502 should WARN: got %s", c.Level)
	}

	// Transport error => WARN, never FAIL.
	if c := apiReach("http://127.0.0.1:1"); c.Level != levelWarn {
		t.Errorf("unreachable should WARN: got %s", c.Level)
	}
}
