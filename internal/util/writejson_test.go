package util

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSONMarshalsBeforeWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, make(chan int))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 after marshal failure", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("500 body is not JSON: %v %q", err, rec.Body.String())
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "internal_error" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWriteJSONSuccessWritesRequestedStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]string{"ok": "yes"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != "yes" {
		t.Fatalf("body = %v", body)
	}
}
