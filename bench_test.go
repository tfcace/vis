package vis_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tfcace/vis"
)

// The read path every request takes: one atomic load, one slice index.
func BenchmarkSnapshotGet(b *testing.B) {
	in := vis.NewInstance()
	for b.Loop() {
		_ = cacheSize.Get(in.Snapshot())
	}
}

func BenchmarkSet(b *testing.B) {
	in := vis.NewInstance()
	v := int64(1)
	for b.Loop() {
		v = v%9999 + 1
		if _, err := in.Set(vis.Change{Knob: "test_cache_size", Value: v, By: sam}); err != nil {
			b.Fatal(err)
		}
	}
}

// What the middleware adds to every request: a snapshot in the context and timing.
func BenchmarkHTTPMiddleware(b *testing.B) {
	in := vis.NewInstance()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k/{key}", func(w http.ResponseWriter, r *http.Request) { _ = cacheSize.From(r.Context()) })
	h := in.HTTP(mux)
	req := httptest.NewRequest("GET", "/k/1", nil)
	w := httptest.NewRecorder()
	for b.Loop() {
		h.ServeHTTP(w, req)
	}
}

func BenchmarkPlainMux(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k/{key}", func(w http.ResponseWriter, r *http.Request) {})
	req := httptest.NewRequest("GET", "/k/1", nil)
	w := httptest.NewRecorder()
	for b.Loop() {
		mux.ServeHTTP(w, req)
	}
}
