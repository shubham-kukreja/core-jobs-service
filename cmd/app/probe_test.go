package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }))
	defer ok.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer down.Close()
	if err := probe(t.Context(), ok.URL+"/readyz"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{down.URL, "https://127.0.0.1/readyz", "http://example.com/readyz", "http://10.0.0.1/", "::bad"} {
		if err := probe(t.Context(), u); err == nil {
			t.Errorf("probe %s succeeded", u)
		}
	}
}
