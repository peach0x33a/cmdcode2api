package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunVendorsAndLeavesUnchangedFileAlone(t *testing.T) {
	payload := catalogPayload(t, 60)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, payload)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "allowances.json")
	if err := run(srv.URL, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	first, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read vendored file: %v", err)
	}
	if want := payload + "\n"; string(first) != want {
		t.Fatalf("vendored file = %q, want %q", first, want)
	}

	old := time.Unix(1000, 0)
	if err := os.Chtimes(out, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := run(srv.URL, out); err != nil {
		t.Fatalf("second run: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatal("run rewrote an unchanged file")
	}
}

func TestRunRejectsNewerSchema(t *testing.T) {
	payload := strings.Replace(catalogPayload(t, 60), `"schema":1`, `"schema":2`, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, payload)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "allowances.json")
	err := run(srv.URL, out)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("run error = %v, want a schema mismatch", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("run wrote a file after rejecting the catalog")
	}
}

func TestValidateRejectsDuplicateID(t *testing.T) {
	c := decodeCatalog(t, catalogPayload(t, 60))
	c.Models[1].ID = c.Models[0].ID
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := validate(data); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("validate error = %v, want a duplicate id error", err)
	}
}

func TestValidateRejectsNonPositiveAllowance(t *testing.T) {
	c := decodeCatalog(t, catalogPayload(t, 60))
	c.Models[0].Pro = 0
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := validate(data); err == nil || !strings.Contains(err.Error(), "non-positive") {
		t.Fatalf("validate error = %v, want a non-positive allowance error", err)
	}
}

func decodeCatalog(t *testing.T, data string) catalogFile {
	t.Helper()
	var c catalogFile
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		t.Fatalf("decode catalog fixture: %v", err)
	}
	return c
}

// catalogPayload builds a catalog document with n valid models and no trailing
// newline, so run must add one.
func catalogPayload(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"schema":1,"source":"https://example.test/docs","captured":"2026-11-16","models":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"model-%02d","name":"Model %02d","category":"opensource","go":6,"goat":20,"pro":30}`, i, i)
	}
	b.WriteString("]}")
	return b.String()
}
