// Command gen-allowances regenerates internal/app/allowances.json from the
// public Command Code pricing page.
//
// The page is a Next.js app: its RSC payload carries the model catalog as plain
// JSON, including the per-plan USD allowances ("boosted usage") we track. This
// command fetches that payload, keeps the fields the app reads, and writes a
// compact catalog with one JSON object per line. The result is still a single
// valid JSON document, so the app keeps using encoding/json, while a change to
// one model touches exactly one line in review.
//
// Run it with:
//
//	go generate ./internal/app
//
// The command needs network access and is never run at build time; the
// checked-in file keeps builds hermetic and offline.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
)

const sourceURL = "https://commandcode.ai/docs/resources/pricing-limits"

// upstreamModel is the part of the page's model object this catalog needs.
type upstreamModel struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Category         string             `json:"category"`
	PlanAllowanceUSD map[string]float64 `json:"planAllowanceUsd"`
}

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
	Source   string         `json:"source"`
	Captured string         `json:"captured"`
	Models   []catalogModel `json:"models"`
}

func main() {
	out := flag.String("out", "allowances.json", "output file")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintf(os.Stderr, "gen-allowances: %v\n", err)
		os.Exit(1)
	}
}

func run(out string) error {
	body, err := fetch(sourceURL)
	if err != nil {
		return err
	}
	models, err := catalogFrom(body)
	if err != nil {
		return err
	}
	// The page lists models newest first, which reshuffles often. Sorting by id
	// keeps regeneration deterministic and makes a new model a one-line diff.
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })

	if unchanged(out, models) {
		fmt.Printf("%s: upstream unchanged, left as is\n", out)
		return nil
	}

	data, err := render(models, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: wrote %d models\n", out, len(models))
	return nil
}

func fetch(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// The RSC payload is a fraction of the rendered HTML and needs no scraping.
	req.Header.Set("RSC", "1")
	req.Header.Set("User-Agent", "cmdcode2api-gen-allowances/1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// catalogFrom pulls the model catalog out of the RSC payload. The payload is
// not JSON itself, but the "models" property is a JSON array, which we locate
// and decode on its own.
func catalogFrom(body []byte) ([]catalogModel, error) {
	raw, err := findModelsArray(body)
	if err != nil {
		return nil, err
	}
	var upstream []upstreamModel
	if err := json.Unmarshal(raw, &upstream); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	if len(upstream) < 50 {
		return nil, fmt.Errorf("decoded only %d models, refusing to overwrite (page layout changed?)", len(upstream))
	}
	return modelsFrom(upstream)
}

// modelsFrom validates the decoded catalog and keeps the fields the app reads.
func modelsFrom(upstream []upstreamModel) ([]catalogModel, error) {
	seen := make(map[string]bool, len(upstream))
	models := make([]catalogModel, 0, len(upstream))
	for _, m := range upstream {
		id := strings.TrimSpace(m.ID)
		name := strings.TrimSpace(m.Name)
		category := strings.TrimSpace(m.Category)
		if id == "" || name == "" || category == "" {
			return nil, fmt.Errorf("model %q has a missing id, name, or category", m.ID)
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate model id %q", id)
		}
		seen[id] = true

		goUSD, ok := m.PlanAllowanceUSD["go"]
		if !ok {
			return nil, fmt.Errorf("model %q has no go allowance", id)
		}
		goatUSD, ok := m.PlanAllowanceUSD["goat"]
		if !ok {
			return nil, fmt.Errorf("model %q has no goat allowance", id)
		}
		proUSD, ok := m.PlanAllowanceUSD["pro"]
		if !ok {
			return nil, fmt.Errorf("model %q has no pro allowance", id)
		}
		models = append(models, catalogModel{
			ID:       id,
			Name:     name,
			Category: category,
			Go:       goUSD,
			Goat:     goatUSD,
			Pro:      proUSD,
		})
	}
	return models, nil
}

// findModelsArray returns the first "models" value that looks like the catalog:
// a JSON array whose entries carry per-plan allowances.
func findModelsArray(body []byte) ([]byte, error) {
	needle := []byte(`"models"`)
	for off := 0; ; {
		i := bytes.Index(body[off:], needle)
		if i < 0 {
			return nil, errors.New(`no "models" array in the response; the page layout changed`)
		}
		i += off
		j := i + len(needle)
		j = skipSpace(body, j)
		if j < len(body) && body[j] == ':' {
			j = skipSpace(body, j+1)
			if j < len(body) && body[j] == '[' {
				if arr, err := jsonArrayAt(body, j); err == nil {
					var probe []upstreamModel
					if json.Unmarshal(arr, &probe) == nil && len(probe) > 0 && probe[0].PlanAllowanceUSD != nil {
						return arr, nil
					}
				}
			}
		}
		off = i + len(needle)
	}
}

// jsonArrayAt returns the balanced JSON array starting at body[start].
func jsonArrayAt(body []byte, start int) ([]byte, error) {
	if start < 0 || start >= len(body) || body[start] != '[' {
		return nil, errors.New("not a JSON array")
	}
	depth := 0
	inString, escaped := false, false
	for i := start; i < len(body); i++ {
		c := body[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return body[start : i+1], nil
			}
		}
	}
	return nil, errors.New("unterminated JSON array")
}

func skipSpace(body []byte, i int) int {
	for i < len(body) {
		switch body[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// unchanged reports whether the checked-in file already holds exactly this
// catalog, so a regeneration with no upstream changes leaves the file and its
// capture date alone.
func unchanged(path string, models []catalogModel) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var old catalogFile
	if json.Unmarshal(data, &old) != nil {
		return false
	}
	return old.Source == sourceURL && old.Captured != "" && reflect.DeepEqual(old.Models, models)
}

func render(models []catalogModel, captured string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"source":`)
	quotedSource, err := json.Marshal(sourceURL)
	if err != nil {
		return nil, err
	}
	b.Write(quotedSource)
	b.WriteString(`,"captured":"`)
	b.WriteString(captured)
	b.WriteString(`","models":[`)
	b.WriteByte('\n')
	for i, m := range models {
		line, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		b.Write(line)
		if i != len(models)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]}\n")
	return b.Bytes(), nil
}
