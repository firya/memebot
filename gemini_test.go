package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// geminiResponse is a helper to build a minimal Gemini JSON response.
func geminiResponse(text string) any {
	return map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{
						map[string]string{"text": text},
					},
				},
			},
		},
	}
}

func serveJSON(t *testing.T, status int, body any, headers ...map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range headers {
			for k, v := range h {
				w.Header().Set(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
}

func TestCallGemini_Success(t *testing.T) {
	srv := serveJSON(t, 200, geminiResponse("  кот сидит  "))
	defer srv.Close()

	got, err := callGemini("key", srv.URL, "", "", []byte("img"), "image/jpeg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "кот сидит" {
		t.Errorf("got %q, want %q", got, "кот сидит")
	}
}

func TestCallGemini_RateLimited_429WithRetryAfter(t *testing.T) {
	srv := serveJSON(t, 429, map[string]any{}, map[string]string{"Retry-After": "30"})
	defer srv.Close()

	_, err := callGemini("key", srv.URL, "", "", []byte("img"), "image/jpeg")
	if !errors.Is(err, errRateLimited) {
		t.Errorf("expected errRateLimited, got %v", err)
	}
}

func TestCallGemini_QuotaExceeded_429NoRetryAfter(t *testing.T) {
	srv := serveJSON(t, 429, map[string]any{})
	defer srv.Close()

	_, err := callGemini("key", srv.URL, "", "", []byte("img"), "image/jpeg")
	if !errors.Is(err, errQuotaExceeded) {
		t.Errorf("expected errQuotaExceeded, got %v", err)
	}
}

func TestCallGemini_APIErrorWithQuota(t *testing.T) {
	body := map[string]any{
		"error": map[string]string{"message": "RESOURCE_EXHAUSTED: quota exceeded"},
	}
	srv := serveJSON(t, 200, body)
	defer srv.Close()

	_, err := callGemini("key", srv.URL, "", "", []byte("img"), "image/jpeg")
	if !errors.Is(err, errQuotaExceeded) {
		t.Errorf("expected errQuotaExceeded, got %v", err)
	}
}

func TestCallGemini_APIErrorWithRetryAfter(t *testing.T) {
	body := map[string]any{
		"error": map[string]string{"message": "RESOURCE_EXHAUSTED: rate limit"},
	}
	srv := serveJSON(t, 429, body, map[string]string{"Retry-After": "60"})
	defer srv.Close()

	_, err := callGemini("key", srv.URL, "", "", []byte("img"), "image/jpeg")
	if !errors.Is(err, errRateLimited) {
		t.Errorf("expected errRateLimited, got %v", err)
	}
}

func TestCallGemini_GenericAPIError(t *testing.T) {
	body := map[string]any{
		"error": map[string]string{"message": "internal server error"},
	}
	srv := serveJSON(t, 500, body)
	defer srv.Close()

	_, err := callGemini("key", srv.URL, "", "", []byte("img"), "image/jpeg")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, errRateLimited) || errors.Is(err, errQuotaExceeded) {
		t.Errorf("generic error should not be rate-limit/quota: %v", err)
	}
}

func TestCallGemini_EmptyResponse(t *testing.T) {
	srv := serveJSON(t, 200, map[string]any{"candidates": []any{}})
	defer srv.Close()

	_, err := callGemini("key", srv.URL, "", "", []byte("img"), "image/jpeg")
	if err == nil {
		t.Fatal("expected error for empty candidates, got nil")
	}
}

func TestCallGemini_WorkerSecret(t *testing.T) {
	var gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("X-Worker-Secret")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(geminiResponse("ok"))
	}))
	defer srv.Close()

	callGemini("key", srv.URL, "mysecret", "", []byte("img"), "image/jpeg")
	if gotSecret != "mysecret" {
		t.Errorf("X-Worker-Secret: got %q, want %q", gotSecret, "mysecret")
	}
}

func TestCallGemini_ModelAndKeyHeader(t *testing.T) {
	for _, model := range []string{"", "gemini-3.5-flash-lite"} {
		t.Run(model, func(t *testing.T) {
			wantModel := model
			if wantModel == "" {
				wantModel = defaultGeminiModel
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1beta/models/"+wantModel+":generateContent" {
					t.Errorf("unexpected model path: %s", r.URL.Path)
				}
				if r.Header.Get("x-goog-api-key") != "test-key" || r.URL.RawQuery != "" {
					t.Error("API key must be sent in its header, never in the URL")
				}
				json.NewEncoder(w).Encode(geminiResponse("ok"))
			}))
			defer srv.Close()
			_, err := callGemini("test-key", srv.URL+"/", "", model, []byte("img"), "image/jpeg")
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCallGemini_BillingFailureIsNotQuota(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests} {
		srv := serveJSON(t, status, map[string]any{
			"error": map[string]string{"message": "Your prepayment credits are depleted. Please go to AI Studio to manage your project and billing."},
		}, map[string]string{"Retry-After": "30"})
		_, err := callGemini("key", srv.URL, "", "", []byte("img"), "image/jpeg")
		srv.Close()
		if !errors.Is(err, errBillingUnavailable) || errors.Is(err, errQuotaExceeded) || errors.Is(err, errRateLimited) {
			t.Errorf("status %d: expected a billing failure, got %v", status, err)
		}
	}
}

func TestNextGeminiQuotaReset(t *testing.T) {
	for _, tc := range []struct{ now, want string }{
		{"2026-10-29T06:30:00Z", "2026-10-29T07:05:00Z"},
		{"2026-10-29T08:00:00Z", "2026-10-30T07:05:00Z"},
		{"2026-11-01T07:30:00Z", "2026-11-02T08:05:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := nextGeminiQuotaReset(now).UTC().Format(time.RFC3339); got != tc.want {
			t.Errorf("reset at %s = %s, want %s", tc.now, got, tc.want)
		}
	}
}

func TestFetchImageBytes_GetFileFails(t *testing.T) {
	srv := serveJSON(t, 200, map[string]any{"ok": false, "description": "file not found"})
	defer srv.Close()

	// Point the TG API client at our mock by temporarily replacing tgAPIClient.
	orig := tgAPIClient
	tgAPIClient = &http.Client{Transport: rewriteHost(srv.URL)}
	defer func() { tgAPIClient = orig }()

	cfg := Config{TelegramToken: "test"}
	_, _, err := fetchImageBytes(cfg, "bad_file_id")
	if err == nil {
		t.Fatal("expected error when getFile returns ok=false")
	}
}

// rewriteHost returns a RoundTripper that sends all requests to baseURL,
// preserving path and query.
type hostRewriter struct {
	base string
}

func rewriteHost(base string) http.RoundTripper { return &hostRewriter{base: base} }

func (h *hostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	newReq := req.Clone(req.Context())
	newReq.URL.Scheme = "http"
	newReq.URL.Host = h.base[len("http://"):]
	return http.DefaultTransport.RoundTrip(newReq)
}
