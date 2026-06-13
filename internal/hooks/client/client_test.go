package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostJSON_Success(t *testing.T) {
	var calls int32
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := PostJSON(srv.URL+"/x", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 call")
	}
	if string(gotBody) != `{"a":1}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestPostJSON_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := PostJSON(srv.URL+"/x", []byte(`{}`)); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestPostJSONCtx_Timeout(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stop
	}))
	t.Cleanup(func() { close(stop); srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := PostJSONCtx(ctx, srv.URL+"/x", []byte(`{}`)); err == nil {
		t.Fatal("expected timeout error")
	}
}
