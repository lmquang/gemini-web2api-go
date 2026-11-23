package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatCancellationReleasesSlot(t *testing.T) {
	resetConversationTestState(t)
	rtMu.Lock()
	rtVal.MultiTurn = true
	rtVal.PerIPConcurrent = 1
	rtVal.PerIPRPM, rtVal.PerIPRPH = 0, 0
	rtMu.Unlock()
	slotsMu.Lock()
	oldSlots := slots
	slots = map[int64]*ipSlot{}
	slotsMu.Unlock()
	t.Cleanup(func() { slotsMu.Lock(); slots = oldSlots; slotsMu.Unlock() })

	entered, stopped, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-time.After(8 * time.Second):
		}
	}))
	defer upstream.Close()
	old := generateConversationOnSlot
	t.Cleanup(func() { generateConversationOnSlot = old })
	raw := conversationTestRaw(t, "slot reused")
	generateConversationOnSlot = func(_ string, mc ModelConfig, _ *convState, delta, _ func(string), _ Proxy) (*StreamResult, error) {
		if attempts.Add(1) == 1 {
			_, _, _, _, err := doGeminiRequest(mc.generationContext(), upstream.URL, "", nil, "", nil)
			return nil, err
		}
		delta("slot reused")
		return &StreamResult{Raw: raw}, nil
	}
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			defer close(returned)
		}
		handleChatCompletions(w, r)
	}))
	defer gateway.Close()
	client := gateway.Client()
	client.Timeout = 5 * time.Second
	send := func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gemini-3.5-flash-lite","stream":true,"messages":[{"role":"user","content":"Hello"}]}`))
		if err != nil {
			return nil, err
		}
		return client.Do(req)
	}
	check := func(status int, markers ...string) {
		t.Helper()
		resp, err := send(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			t.Fatalf("HTTP %d, want %d: %s", resp.StatusCode, status, body)
		}
		for _, marker := range markers {
			if !strings.Contains(string(body), marker) {
				t.Fatalf("missing %q: %s", marker, body)
			}
		}
	}
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		if resp, err := send(ctx); err == nil {
			resp.Body.Close()
		}
	}()
	wait(entered, "upstream entry")
	check(http.StatusTooManyRequests, "ip_slot_full")
	cancel()
	wait(clientDone, "HTTP client cancellation")
	wait(stopped, "upstream cancellation")
	wait(returned, "bridge handler return")
	check(http.StatusOK, "slot reused", "data: [DONE]")
	if got := attempts.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2", got)
	}
}
