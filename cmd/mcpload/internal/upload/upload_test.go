package upload

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runs" {
			http.Error(w, "bad route", 404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer k1" {
			http.Error(w, "unauthorized", 401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"schemaVersion":"1"}` {
			http.Error(w, "bad body", 400)
			return
		}
		w.Write([]byte(`{"runId":"r1","status":"fail","regression":true,"url":"https://x/runs/r1"}`))
	}))
	defer srv.Close()
	c := &Client{}
	r, _, err := c.Upload(context.Background(), srv.URL+"/", "k1", []byte(`{"schemaVersion":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.RunID != "r1" || r.Status != "fail" || string(r.Regression) != "true" || r.URL != "https://x/runs/r1" {
		t.Errorf("resp = %+v", r)
	}
	if _, _, err := c.Upload(context.Background(), srv.URL, "bad", []byte(`{}`)); err == nil {
		t.Error("want 401 error")
	}
	if _, _, err := c.Upload(context.Background(), srv.URL, "", nil); err == nil {
		t.Error("want missing key error")
	}
}
