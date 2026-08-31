package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	discordDeliveryTimeout   = 5 * time.Second
	discordStateLimit        = 512
	discordStateMaxAge       = 180 * 24 * time.Hour
	defaultDiscordHourlyCap  = 3
	defaultDiscordWeeklyCap  = 6
	defaultDiscordMonthlyCap = 10
	sessionExpiryAlertWindow = 24 * time.Hour
)

var discordClient = &http.Client{Timeout: discordDeliveryTimeout}

type discordAlertState struct {
	Sent  map[string]time.Time    `json:"sent"`
	Level map[string]discordLevel `json:"level,omitempty"`
}

type discordLevel struct {
	Threshold int       `json:"threshold"`
	SentAt    time.Time `json:"sent_at"`
}

type DiscordAlerter struct {
	mu              sync.Mutex
	webhook         string
	path            string
	state           discordAlertState
	inFlight        map[string]bool
	hourlyCap       float64
	weeklyCap       float64
	monthlyCap      float64
	mentionEveryone bool
}

func NewDiscordAlerter(webhook, statePath string) *DiscordAlerter {
	return NewDiscordAlerterWithConfig(DiscordAlertConfig{WebhookURL: webhook, StateFile: statePath})
}

func NewDiscordAlerterWithConfig(config DiscordAlertConfig) *DiscordAlerter {
	if config.HourlyCap == 0 {
		config.HourlyCap = defaultDiscordHourlyCap
	}
	if config.WeeklyCap == 0 {
		config.WeeklyCap = defaultDiscordWeeklyCap
	}
	if config.MonthlyCap == 0 {
		config.MonthlyCap = defaultDiscordMonthlyCap
	}
	webhook, statePath := config.WebhookURL, config.StateFile
	if statePath == "" {
		statePath = "discord-alerts.json"
	}
	a := &DiscordAlerter{webhook: webhook, path: statePath, hourlyCap: config.HourlyCap, weeklyCap: config.WeeklyCap, monthlyCap: config.MonthlyCap, mentionEveryone: config.MentionEveryone, state: discordAlertState{Sent: make(map[string]time.Time), Level: make(map[string]discordLevel)}, inFlight: make(map[string]bool)}
	a.load()
	return a
}

func (a *DiscordAlerter) Evaluate(rows []AccountBilling) {
	if a == nil || a.webhook == "" {
		return
	}
	// Snapshot evaluation is intentionally done as one batch, so an unknown or
	// stale account can never be treated as zero usage for suppression.
	fresh := func(row AccountBilling) bool {
		return row.FetchedAt != nil && time.Since(*row.FetchedAt) <= discordDeliveryTimeout*2 && row.LastError == ""
	}
	for _, row := range rows {
		if !fresh(row) || row.Credits == nil {
			continue
		}
		for _, metric := range []struct {
			name  string
			win   BillingWindow
			used  float64
			cap   float64
			reset int64
		}{
			{"five-hour", row.Credits.WindowLimits.FiveHour, row.Credits.WindowLimits.FiveHour.Used, a.hourlyCap, row.Credits.WindowLimits.FiveHour.ResetAt},
			{"weekly", row.Credits.WindowLimits.Weekly, row.Credits.WindowLimits.Weekly.Used, a.weeklyCap, row.Credits.WindowLimits.Weekly.ResetAt},
			{"monthly", BillingWindow{}, monthlyUsed(row.Credits.Credits.MonthlyCredits, a.monthlyCap), a.monthlyCap, 0},
		} {
			cap := metric.cap
			if cap <= 0 {
				continue
			}
			percentage := metric.used / cap * 100
			threshold := highestDiscordThreshold(percentage)
			key := fmt.Sprintf("usage|%s|%s|%d", row.Account, metric.name, metric.reset)
			if threshold == 0 {
				a.clearUsageLevel(key)
				continue
			}
			if a.otherUsed(rows, row.Account, metric.name, fresh) {
				continue
			}
			a.deliverUsage(key, threshold, fmt.Sprintf("⚠️ %s account %q is at %.1f%% of its configured %s cap (%g/%g).", metric.name, row.Account, percentage, metric.name, metric.used, cap))
		}
	}
	for _, row := range rows {
		if !fresh(row) {
			continue
		}
		if row.PlanExpiresAt != nil {
			if end := *row.PlanExpiresAt; !end.Before(time.Now()) && !end.After(time.Now().Add(7*24*time.Hour)) {
				key := fmt.Sprintf("expiration|%s|%s", row.Account, end.UTC().Format(time.RFC3339))
				a.deliver(key, fmt.Sprintf("⚠️ Subscription for account %q ends on %s.", row.Account, end.UTC().Format(time.RFC3339)))
			}
		}
		if row.SessionExpiresAt != nil && !row.SessionExpiresAt.Before(time.Now()) && !row.SessionExpiresAt.After(time.Now().Add(sessionExpiryAlertWindow)) {
			expires := row.SessionExpiresAt.UTC().Format(time.RFC3339)
			a.deliver(fmt.Sprintf("session-expiration|%s|%s", row.Account, expires), fmt.Sprintf("⚠️ Session token for account %q expires on %s.", row.Account, expires))
		}
	}
}

// monthlyUsed converts the API's remaining monthly balance into consumed
// credits. Values outside the configured range are clamped defensively.
func monthlyUsed(remaining, cap float64) float64 {
	used := cap - remaining
	if used < 0 {
		return 0
	}
	if used > cap {
		return cap
	}
	return used
}

func highestDiscordThreshold(percentage float64) int {
	switch {
	case percentage >= 100:
		return 100
	case percentage >= 95:
		return 95
	case percentage >= 90:
		return 90
	default:
		return 0
	}
}

func (a *DiscordAlerter) otherUsed(rows []AccountBilling, account, metric string, fresh func(AccountBilling) bool) bool {
	for _, row := range rows {
		if row.Account == account || !fresh(row) || row.Credits == nil {
			continue
		}
		used := row.Credits.WindowLimits.Weekly.Used
		if metric == "five-hour" {
			used = row.Credits.WindowLimits.FiveHour.Used
		} else if metric == "monthly" {
			used = monthlyUsed(row.Credits.Credits.MonthlyCredits, a.monthlyCap)
		}
		if used > 0 {
			return true
		}
	}
	return false
}

func (a *DiscordAlerter) deliver(key, content string) {
	content = a.alertContent(content)
	a.mu.Lock()
	if _, ok := a.state.Sent[key]; ok || a.inFlight[key] {
		a.mu.Unlock()
		return
	}
	a.inFlight[key] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.inFlight, key)
		a.mu.Unlock()
	}()
	a.post(content, func() {
		a.mu.Lock()
		previous := cloneDiscordAlertState(a.state)
		at := time.Now().UTC()
		a.state.Sent[key] = at
		a.pruneLocked()
		if err := a.saveLocked(); err != nil {
			a.state = previous
		}
		a.mu.Unlock()
	})
}

func (a *DiscordAlerter) deliverUsage(key string, threshold int, content string) {
	content = a.alertContent(content)
	a.mu.Lock()
	if a.state.Level[key].Threshold >= threshold || a.inFlight[key] {
		a.mu.Unlock()
		return
	}
	a.inFlight[key] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.inFlight, key)
		a.mu.Unlock()
	}()
	a.post(content, func() {
		a.mu.Lock()
		if a.state.Level[key].Threshold < threshold {
			previousState := cloneDiscordAlertState(a.state)
			previous := a.state.Level[key]
			a.state.Level[key] = discordLevel{Threshold: threshold, SentAt: time.Now().UTC()}
			a.pruneLocked()
			if err := a.saveLocked(); err != nil {
				a.state = previousState
				_ = previous
			}
		}
		a.mu.Unlock()
	})
}

func (a *DiscordAlerter) alertContent(content string) string {
	if a.mentionEveryone {
		return "@everyone " + content
	}
	return content
}

func (a *DiscordAlerter) post(content string, onSuccess func()) {
	body, _ := json.Marshal(map[string]string{"content": content})
	ctx, cancel := context.WithTimeout(context.Background(), discordDeliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.webhook, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := discordClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		onSuccess()
	}
}

func (a *DiscordAlerter) clearUsageLevel(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.state.Level[key]; !ok {
		return
	}
	previous := a.state.Level[key]
	previousState := cloneDiscordAlertState(a.state)
	delete(a.state.Level, key)
	if err := a.saveLocked(); err != nil {
		a.state = previousState
		_ = previous
	}
}

func cloneDiscordAlertState(state discordAlertState) discordAlertState {
	clone := discordAlertState{Sent: make(map[string]time.Time, len(state.Sent)), Level: make(map[string]discordLevel, len(state.Level))}
	for key, value := range state.Sent {
		clone.Sent[key] = value
	}
	for key, value := range state.Level {
		clone.Level[key] = value
	}
	return clone
}

func (a *DiscordAlerter) load() {
	b, err := os.ReadFile(a.path)
	if err == nil {
		if err := json.Unmarshal(b, &a.state); err != nil {
			log.Printf("[WARN] discord alert state %q: invalid JSON: %v", a.path, err)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("[WARN] discord alert state %q: read failed: %v", a.path, err)
	}
	if a.state.Sent == nil {
		a.state.Sent = make(map[string]time.Time)
	}
	if a.state.Level == nil {
		a.state.Level = make(map[string]discordLevel)
	}
	a.mu.Lock()
	a.pruneLocked()
	a.mu.Unlock()
}

func (a *DiscordAlerter) pruneLocked() {
	cutoff := time.Now().Add(-discordStateMaxAge)
	for key, at := range a.state.Sent {
		if at.Before(cutoff) {
			delete(a.state.Sent, key)
		}
	}
	for key, level := range a.state.Level {
		if level.SentAt.Before(cutoff) {
			delete(a.state.Level, key)
		}
	}
	if len(a.state.Level) > discordStateLimit {
		keys := make([]string, 0, len(a.state.Level))
		for key := range a.state.Level {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return a.state.Level[keys[i]].SentAt.Before(a.state.Level[keys[j]].SentAt) })
		for _, key := range keys[:len(keys)-discordStateLimit] {
			delete(a.state.Level, key)
		}
	}
	if len(a.state.Sent) <= discordStateLimit {
		return
	}
	keys := make([]string, 0, len(a.state.Sent))
	for key := range a.state.Sent {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return a.state.Sent[keys[i]].Before(a.state.Sent[keys[j]]) })
	for _, key := range keys[:len(keys)-discordStateLimit] {
		delete(a.state.Sent, key)
	}
}

func (a *DiscordAlerter) saveLocked() (err error) {
	defer func() {
		if err != nil {
			log.Printf("[WARN] discord alert state %q: save failed: %v", a.path, err)
		}
	}()
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0700); err != nil && filepath.Dir(a.path) != "." {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.path), ".discord-alerts-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, a.path); err != nil {
		return err
	}
	// Apply the mode to the final path as well. On some platforms, including
	// Windows, the temporary file's mode is not preserved by Rename.
	return os.Chmod(a.path, 0600)
}
