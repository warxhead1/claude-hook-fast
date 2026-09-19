package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSteeringPollCancelsHeadersAndBodyWithinHookBudget(t *testing.T) {
	for _, partialBody := range []bool{false, true} {
		t.Run(fmt.Sprint(partialBody), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			canceled := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if partialBody {
					_, _ = fmt.Fprint(w, `[{"created_at":"2026-09-19T12:00:00Z","message":"incomplete"}`)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(canceled)
			}))
			defer srv.Close()
			t.Setenv("HEARTH_LOOM_URL", srv.URL)
			start := time.Now()
			withStdin(t, `{"session_id":"deadline-test"}`, cmdSteeringPoll)
			if elapsed := time.Since(start); elapsed >= time.Second {
				t.Errorf("steering exhausted outer hook budget: %v", elapsed)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("request was not canceled")
			}
			if cursor := readSteeringCursor("deadline-test"); cursor != 0 {
				t.Fatalf("partial response advanced cursor: %d", cursor)
			}
		})
	}
}
