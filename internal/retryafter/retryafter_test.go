package retryafter

import (
	"net/http"
	"testing"
	"time"
)

// U1: Parse of the header value.
func TestU1Parse(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"7", 7 * time.Second, true},
		{"0", time.Second, true},
		{"-3", time.Second, true},
		{"99999", time.Hour, true},
		{"99999999999999999999999", time.Hour, true},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), time.Second, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tt := range tests {
		got, ok := Parse(tt.in, now)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Parse(%q) = %v, %v, want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestFromStatus(t *testing.T) {
	now := time.Now()
	mk := func(code int, h string) (time.Duration, bool) {
		hdr := http.Header{}
		if h != "" {
			hdr.Set("Retry-After", h)
		}
		return FromStatus(code, hdr, now)
	}
	if d, ok := mk(429, "7"); !ok || d != 7*time.Second {
		t.Errorf("429 with value: %v %v", d, ok)
	}
	if d, ok := mk(429, ""); !ok || d != 0 {
		t.Errorf("429 without value: %v %v", d, ok)
	}
	if d, ok := mk(503, "5"); !ok || d != 5*time.Second {
		t.Errorf("503 with value: %v %v", d, ok)
	}
	if _, ok := mk(503, ""); ok {
		t.Error("503 without value")
	}
	if _, ok := mk(500, "5"); ok {
		t.Error("500")
	}
}
