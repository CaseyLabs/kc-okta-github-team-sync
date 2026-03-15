package run

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CaseyLabs/okta-github-team-sync/internal/github"
	"github.com/CaseyLabs/okta-github-team-sync/internal/okta"
)

func TestParseGroupName(t *testing.T) {
	prefix := "Okta-Team-"
	tests := []struct {
		name      string
		wantName  string
		wantRole  string
		wantError bool
	}{
		{"Okta-Team-Platform", "Okta-Team-Platform", "", false},
		{"Okta-Team-Platform__maintain", "Okta-Team-Platform", "maintain", false},
		{"Okta-Team-Platform:Admin", "Okta-Team-Platform", "Admin", false},
		{"Okta-Team-", "", "", true},
		{"Other-Platform", "", "", true},
	}

	for _, tt := range tests {
		gotName, gotRole, err := parseGroupName(tt.name, prefix)
		if tt.wantError {
			if err == nil {
				t.Fatalf("parseGroupName(%q) expected error", tt.name)
			}
			continue
		}

		if err != nil {
			t.Fatalf("parseGroupName(%q) unexpected error: %v", tt.name, err)
		}

		if gotName != tt.wantName || gotRole != tt.wantRole {
			t.Fatalf("parseGroupName(%q) = (%q,%q), want (%q,%q)", tt.name, gotName, gotRole, tt.wantName, tt.wantRole)
		}
	}
}

func TestMapRoleToPermission(t *testing.T) {
	tests := []struct {
		role       string
		want       string
		shouldHave bool
	}{
		{"admin", "admin", true},
		{"maintain", "maintain", true},
		{"write", "push", true},
		{"VIEWER", "pull", true},
		{"triage", "triage", true},
		{"unknown", "", false},
	}

	for _, tt := range tests {
		got, ok := mapRoleToPermission(tt.role)
		if ok != tt.shouldHave || got != tt.want {
			t.Fatalf("mapRoleToPermission(%q) = (%q,%v), want (%q,%v)", tt.role, got, ok, tt.want, tt.shouldHave)
		}
	}
}

func TestAppendMappingIfMissing(t *testing.T) {
	existing := []github.Mapping{{GroupID: "g1"}}
	candidate := github.Mapping{GroupID: "g2"}

	out, added := appendMappingIfMissing(existing, candidate)
	if !added {
		t.Fatalf("expected new mapping to be added")
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(out))
	}

	// Re-adding should not increase length.
	out, added = appendMappingIfMissing(out, candidate)
	if added {
		t.Fatalf("expected duplicate mapping to be ignored")
	}
	if len(out) != 2 {
		t.Fatalf("expected mapping count to remain 2, got %d", len(out))
	}
}

func TestEnsureGroupMapping_RefreshesWhenDisabled(t *testing.T) {
	logger := slog.New(&recordingHandler{})
	gh := &fakeGitHubClient{
		mappings: map[string][]github.Mapping{
			"org-a/test-team-alpha": {
				{GroupID: "g1", GroupName: "Okta-Team-Alpha", GroupDescription: "desc", Status: "disabled"},
			},
		},
	}

	group := okta.Group{ID: "g1", Name: "Okta-Team-Alpha", Description: "desc"}
	if err := ensureGroupMapping(context.Background(), gh, "org-a", "test-team-alpha", group, logger); err != nil {
		t.Fatalf("ensureGroupMapping error: %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.patchCalls["org-a/test-team-alpha"] != 1 {
		t.Fatalf("expected patch to refresh disabled mapping")
	}
}

func TestEnsureGroupMapping_NoPatchWhenSynced(t *testing.T) {
	logger := slog.New(&recordingHandler{})
	gh := &fakeGitHubClient{
		mappings: map[string][]github.Mapping{
			"org-a/test-team-alpha": {
				{GroupID: "g1", GroupName: "Okta-Team-Alpha", GroupDescription: "desc", Status: "synced"},
			},
		},
	}

	group := okta.Group{ID: "g1", Name: "Okta-Team-Alpha", Description: "desc"}
	if err := ensureGroupMapping(context.Background(), gh, "org-a", "test-team-alpha", group, logger); err != nil {
		t.Fatalf("ensureGroupMapping error: %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.patchCalls["org-a/test-team-alpha"] != 0 {
		t.Fatalf("expected no patch when mapping already synced")
	}
}

func TestDeriveCandidateGroups_FirstRunProcessesAll(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	groups := []okta.Group{{ID: "g1", Name: "Okta-Team-A"}, {ID: "g2", Name: "Okta-Team-B"}}

	candidates := deriveCandidateGroups(groups, nil, "", false, logger)
	if len(candidates) != len(groups) {
		t.Fatalf("expected %d candidates on first run, got %d", len(groups), len(candidates))
	}
}

func TestDeriveCandidateGroups_SubsequentRunUsesEvents(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	groups := []okta.Group{{ID: "g1", Name: "Okta-Team-A"}, {ID: "g2", Name: "Okta-Team-B"}}
	events := []okta.Event{{Targets: []okta.EventTarget{{Type: "GROUP", ID: "g2"}}}}

	candidates := deriveCandidateGroups(groups, events, "cursor", false, logger)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate from events, got %d", len(candidates))
	}
	if candidates[0].ID != "g2" {
		t.Fatalf("expected candidate g2, got %s", candidates[0].ID)
	}
}

func TestDeriveCandidateGroups_MissingGroupInEvents(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	events := []okta.Event{{Targets: []okta.EventTarget{{Type: "group", ID: "missing"}}}}

	candidates := deriveCandidateGroups(nil, events, "cursor", false, logger)
	if len(candidates) != 0 {
		t.Fatalf("expected no candidates when event group is missing, got %d", len(candidates))
	}
}

func TestReconcile_RetainsCursorOnErrors(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "cursor.json")
	if err := SaveCursor(statePath, "cursor-prev"); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	okta := &fakeOktaClient{
		events: []okta.Event{
			{Targets: []okta.EventTarget{{Type: "group", ID: "g1"}}},
		},
		groups: []okta.Group{{ID: "g1", Name: "Okta-Team-Alpha", Description: "Alpha"}},
		next:   "cursor-next",
	}

	gh := &fakeGitHubClient{
		ensureErr: map[string]error{"org-a": errors.New("boom")},
	}

	cfg := Config{
		Organizations: []string{"org-a"},
		GroupPrefix:   "Okta-Team-",
		StatePath:     statePath,
		MaxWorkers:    1,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := reconcile(context.Background(), gh, okta, cfg, logger)
	if err == nil {
		t.Fatalf("expected error from reconcile")
	}

	cursor, err := LoadCursor(statePath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if cursor != "cursor-prev" {
		t.Fatalf("expected cursor to remain cursor-prev, got %s", cursor)
	}
}

func TestReconcile_PersistsCursorOnSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "cursor.json")
	if err := SaveCursor(statePath, "cursor-prev"); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	okta := &fakeOktaClient{
		events: []okta.Event{
			{Targets: []okta.EventTarget{{Type: "group", ID: "g1"}}},
		},
		groups: []okta.Group{{ID: "g1", Name: "Okta-Team-Beta", Description: "Beta"}},
		next:   "cursor-next",
	}

	gh := &fakeGitHubClient{
		mappings: make(map[string][]github.Mapping),
	}

	cfg := Config{
		Organizations: []string{"org-a"},
		GroupPrefix:   "Okta-Team-",
		StatePath:     statePath,
		MaxWorkers:    1,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := reconcile(context.Background(), gh, okta, cfg, logger); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	cursor, err := LoadCursor(statePath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if cursor != "cursor-next" {
		t.Fatalf("expected cursor to advance to cursor-next, got %s", cursor)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.mappings["org-a/test-team-beta"]) == 0 {
		t.Fatalf("expected mapping to be stored for org-a/test-team-beta")
	}
}

func TestReconcile_AssignsGroupToOktaApp(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "cursor.json")

	oktaClient := &fakeOktaClient{
		events: []okta.Event{
			{Targets: []okta.EventTarget{{Type: "group", ID: "g1"}}},
		},
		groups: []okta.Group{
			{ID: "g1", Name: "Okta-Team-Delta", Description: "Delta"},
		},
		next: "cursor-next",
	}

	ghClient := &fakeGitHubClient{mappings: make(map[string][]github.Mapping)}

	cfg := Config{
		Organizations: []string{"org-a"},
		GroupPrefix:   "Okta-Team-",
		StatePath:     statePath,
		MaxWorkers:    1,
		OktaAppIDs:    []string{"0oa123", "0oa456"},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := reconcile(context.Background(), ghClient, oktaClient, cfg, logger); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	oktaClient.mu.Lock()
	defer oktaClient.mu.Unlock()
	if len(oktaClient.assigned) != 2 {
		t.Fatalf("expected two assignments, got %d", len(oktaClient.assigned))
	}
	seen := make(map[string]struct{})
	for _, entry := range oktaClient.assigned {
		if entry.group != "g1" {
			t.Fatalf("unexpected group assignment: %v", entry)
		}
		seen[entry.app] = struct{}{}
	}
	if _, ok := seen["0oa123"]; !ok {
		t.Fatalf("missing assignment for 0oa123: %#v", oktaClient.assigned)
	}
	if _, ok := seen["0oa456"]; !ok {
		t.Fatalf("missing assignment for 0oa456: %#v", oktaClient.assigned)
	}
}

func TestReconcile_FallbackWhenEventsUnavailable(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "cursor.json")
	if err := SaveCursor(statePath, "cursor-prev"); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	okta := &fakeOktaClient{
		err: &okta.SystemLogUnavailableError{Status: http.StatusTooManyRequests},
		groups: []okta.Group{
			{ID: "g1", Name: "Okta-Team-Gamma", Description: "Gamma"},
			{ID: "g2", Name: "Okta-Team-Delta", Description: "Delta"},
		},
		next: "cursor-next",
	}

	gh := &fakeGitHubClient{mappings: make(map[string][]github.Mapping)}

	cfg := Config{
		Organizations: []string{"org-a", "org-b"},
		GroupPrefix:   "Okta-Team-",
		StatePath:     statePath,
		MaxWorkers:    1,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := reconcile(context.Background(), gh, okta, cfg, logger); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	cursor, err := LoadCursor(statePath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if cursor != "cursor-prev" {
		t.Fatalf("expected cursor to remain previous during fallback, got %s", cursor)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.mappings["org-a/test-team-gamma"]) == 0 {
		t.Fatalf("expected org-a mapping to be created during fallback")
	}
	if len(gh.mappings["org-b/test-team-gamma"]) == 0 {
		t.Fatalf("expected org-b mapping to be created during fallback")
	}
}

func TestReconcile_LogsProgressPerGroup(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "cursor.json")

	oktaClient := &fakeOktaClient{
		events: []okta.Event{
			{Targets: []okta.EventTarget{{Type: "group", ID: "g1"}}},
			{Targets: []okta.EventTarget{{Type: "group", ID: "g2"}}},
		},
		groups: []okta.Group{
			{ID: "g1", Name: "Okta-Team-Alpha", Description: "Alpha"},
			{ID: "g2", Name: "Okta-Team-Beta", Description: "Beta"},
		},
		next: "cursor-next",
	}

	ghClient := &fakeGitHubClient{mappings: make(map[string][]github.Mapping)}

	handler := &recordingHandler{}
	logger := slog.New(handler)

	cfg := Config{
		Organizations: []string{"org-a"},
		GroupPrefix:   "Okta-Team-",
		StatePath:     statePath,
		MaxWorkers:    1,
	}

	if err := reconcile(context.Background(), ghClient, oktaClient, cfg, logger); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	records := handler.Records()
	if len(records) == 0 {
		t.Fatalf("expected progress logs to be recorded")
	}

	startMessages := 0
	finishMessages := 0
	progressValues := make(map[string]struct{})

	for _, record := range records {
		switch record.message {
		case "group processing started":
			startMessages++
		case "group processing finished":
			finishMessages++
			if progress, ok := record.attrs["progress"].(string); ok {
				progressValues[progress] = struct{}{}
			}
		}
	}

	if startMessages != 2 {
		t.Fatalf("expected 2 start logs, got %d", startMessages)
	}
	if finishMessages != 2 {
		t.Fatalf("expected 2 finish logs, got %d", finishMessages)
	}

	if len(progressValues) != 2 {
		t.Fatalf("expected 2 unique progress values, got %d", len(progressValues))
	}
	if _, ok := progressValues["1/2"]; !ok {
		t.Fatalf("missing progress value 1/2")
	}
	if _, ok := progressValues["2/2"]; !ok {
		t.Fatalf("missing progress value 2/2")
	}
}

type fakeOktaClient struct {
	mu       sync.Mutex
	events   []okta.Event
	groups   []okta.Group
	next     string
	err      error
	assigned []struct {
		app   string
		group string
	}
}

func (f *fakeOktaClient) FetchSystemLogDelta(ctx context.Context, cursor *string, lookback time.Duration) ([]okta.Event, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return f.events, f.next, nil
}

func (f *fakeOktaClient) ListGroupsByPrefix(ctx context.Context, prefix string) ([]okta.Group, error) {
	return f.groups, nil
}

func (f *fakeOktaClient) AssignGroupToApp(ctx context.Context, appID, groupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assigned = append(f.assigned, struct {
		app   string
		group string
	}{app: appID, group: groupID})
	return nil
}

type fakeGitHubClient struct {
	mu          sync.Mutex
	ensureErr   map[string]error
	mappings    map[string][]github.Mapping
	permissions map[string]string
	patchCalls  map[string]int
}

func (f *fakeGitHubClient) EnsureTeam(ctx context.Context, org, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ensureErr[org]; err != nil {
		return "", err
	}
	slug := "test-team-" + strings.ToLower(strings.TrimPrefix(name, "Okta-Team-"))
	return slug, nil
}

func (f *fakeGitHubClient) TeamExists(ctx context.Context, org, slug string) (bool, error) {
	return true, nil
}

func (f *fakeGitHubClient) GetGroupMappings(ctx context.Context, org, slug string) ([]github.Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mappings == nil {
		f.mappings = make(map[string][]github.Mapping)
	}
	key := org + "/" + slug
	out := append([]github.Mapping(nil), f.mappings[key]...)
	return out, nil
}

func (f *fakeGitHubClient) PatchGroupMappings(ctx context.Context, org, slug string, groups []github.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mappings == nil {
		f.mappings = make(map[string][]github.Mapping)
	}
	key := org + "/" + slug
	f.mappings[key] = append([]github.Mapping(nil), groups...)
	if f.patchCalls == nil {
		f.patchCalls = make(map[string]int)
	}
	f.patchCalls[key]++
	return nil
}

func (f *fakeGitHubClient) SetTeamPermission(ctx context.Context, org, slug, permission string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.permissions == nil {
		f.permissions = make(map[string]string)
	}
	f.permissions[org+"/"+slug] = permission
	return nil
}

type logRecord struct {
	level   slog.Level
	message string
	attrs   map[string]any
}

type recordingHandler struct {
	mu        sync.Mutex
	records   []logRecord
	baseAttrs []slog.Attr
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	entry := logRecord{
		level:   record.Level,
		message: record.Message,
		attrs:   make(map[string]any),
	}

	for _, attr := range h.baseAttrs {
		entry.attrs[attr.Key] = attr.Value.Any()
	}

	record.Attrs(func(attr slog.Attr) bool {
		entry.attrs[attr.Key] = attr.Value.Any()
		return true
	})

	h.records = append(h.records, entry)
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()

	newBase := append(append([]slog.Attr{}, h.baseAttrs...), attrs...)
	return &recordingHandler{baseAttrs: newBase}
}

func (h *recordingHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *recordingHandler) Records() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]logRecord, len(h.records))
	copy(out, h.records)
	return out
}
