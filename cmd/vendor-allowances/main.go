// Command vendor-allowances replaces internal/app/allowances.json with the
// catalog published by the commandcode-catalog repository.
//
// That repository generates the catalog from the Command Code pricing page. The
// app vendors a copy so builds stay offline and a bad upstream deploy cannot
// affect an already-released binary; only the catalog's schema is trusted at
// vendoring time.
//
// Run it with:
//
//	go generate ./internal/app
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	catalogURL    = "https://raw.githubusercontent.com/peach0x33a/commandcode-catalog/main/catalog.json"
	schemaVersion = 1
	minModels     = 50
)

// catalogModel is the on-disk row. Field order is the order written to the file.
type catalogModel struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Go       float64 `json:"go"`
	Goat     float64 `json:"goat"`
	Pro      float64 `json:"pro"`
}

type catalogFile struct {
	Schema   int            `json:"schema"`
	Source   string         `json:"source"`
	Captured string         `json:"captured"`
	Models   []catalogModel `json:"models"`
}

func main() {
	out := flag.String("out", "allowances.json", "output file")
	url := flag.String("url", catalogURL, "catalog URL")
	flag.Parse()
	if err := run(*url, *out); err != nil {
		fmt.Fprintf(os.Stderr, "vendor-allowances: %v\n", err)
		os.Exit(1)
	}
}

func run(url, out string) error {
	data, err := fetch(url)
	if err != nil {
		return err
	}
	catalog, err := validate(data)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}

	// The published file is already the compact one-object-per-line document the
	// app embeds, so keep it verbatim; only the trailing newline is normalized.
	if old, err := os.ReadFile(out); err == nil && bytes.Equal(old, data) {
		fmt.Printf("%s: up to date (%d models, captured %s)\n", out, len(catalog.Models), catalog.Captured)
		return nil
	}

	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	fmt.Printf("%s: vendored %d models (captured %s)\n", out, len(catalog.Models), catalog.Captured)
	return nil
}

// fetch retries transient transport failures; a cold connection to the raw host
// occasionally dies on the first try.
func fetch(url string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		data, err := fetchOnce(url)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("GET %s: %w", url, lastErr)
}

func fetchOnce(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cmdcode2api-vendor-allowances/1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// validate checks the published catalog before it is allowed into the binary.
// It is deliberately strict: keeping the previous snapshot is always better than
// embedding a malformed or silently changed one.
func validate(data []byte) (*catalogFile, error) {
	var c catalogFile
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	if c.Schema < 1 {
		return nil, fmt.Errorf("catalog has no schema version")
	}
	if c.Schema > schemaVersion {
		return nil, fmt.Errorf("catalog schema %d is newer than the supported schema %d; upgrade the binary", c.Schema, schemaVersion)
	}
	if c.Source == "" {
		return nil, fmt.Errorf("catalog has no source")
	}
	if _, err := time.Parse("2006-01-02", c.Captured); err != nil {
		return nil, fmt.Errorf("catalog captured date %q is not YYYY-MM-DD", c.Captured)
	}
	if len(c.Models) < minModels {
		return nil, fmt.Errorf("catalog has only %d models, expected at least %d", len(c.Models), minModels)
	}

	seen := make(map[string]bool, len(c.Models))
	for _, m := range c.Models {
		if m.ID == "" || m.Name == "" || m.Category == "" {
			return nil, fmt.Errorf("model %q has a missing id, name, or category", m.ID)
		}
		if seen[m.ID] {
			return nil, fmt.Errorf("duplicate model id %q", m.ID)
		}
		seen[m.ID] = true
		if m.Go <= 0 || m.Goat <= 0 || m.Pro <= 0 {
			return nil, fmt.Errorf("model %q has a non-positive allowance", m.ID)
		}
	}
	return &c, nil
}
