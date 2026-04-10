package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChannelMsgSig(t *testing.T) {
	m := channelMsg{id: 42, chatID: -100123}
	sig, chatID := m.MessageSig()
	if sig != "42" {
		t.Errorf("MessageSig: got sig=%q, want \"42\"", sig)
	}
	if chatID != -100123 {
		t.Errorf("MessageSig: got chatID=%d, want -100123", chatID)
	}
}

func TestResolveChannelID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"id": int64(-1001234567890)},
		})
	}))
	defer srv.Close()

	// resolveChannelID builds its own URL, so we pass a fake token and intercept
	// by temporarily overriding the transport used for getChat.
	// Instead, call the underlying HTTP directly against the test server.
	id, err := resolveChannelIDURL(srv.URL, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != -1001234567890 {
		t.Errorf("got ID %d, want -1001234567890", id)
	}
}

func TestResolveChannelID_NotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"description": "chat not found",
		})
	}))
	defer srv.Close()

	_, err := resolveChannelIDURL(srv.URL, "")
	if err == nil {
		t.Fatal("expected error for ok=false response")
	}
}

func TestResolveChannelID_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := resolveChannelIDURL(srv.URL, "")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestResolveChannelID_NetworkError(t *testing.T) {
	_, err := resolveChannelID("token", "@nonexistent_localhost_xyz_12345", "", "")
	if err == nil {
		t.Fatal("expected error for unreachable host")
	}
}
