package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupGitHubTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("OKTA_TEAM_SYNC_GITHUB_TOKEN", "")
	return server
}

func TestEnsureTeam_Existing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/test-org/teams/test-team", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(teamResponse{Slug: "test-team", ID: 123, Name: "Test Team"})
	})

	setupGitHubTestServer(t, mux.ServeHTTP)
	t.Setenv("OKTA_TEAM_SYNC_GITHUB_TOKEN", "test-token")

	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	slug, err := client.EnsureTeam(context.Background(), "test-org", "Test Team")
	if err != nil {
		t.Fatalf("EnsureTeam error: %v", err)
	}
	if slug != "test-team" {
		t.Fatalf("expected slug test-team, got %s", slug)
	}
}

func TestEnsureTeam_CreatesWhenMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/test-org/teams/test-team", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/orgs/test-org/teams", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload["name"] != "Test Team" {
			t.Fatalf("unexpected body: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(teamResponse{Slug: "test-team", ID: 456, Name: "Test Team"})
	})

	setupGitHubTestServer(t, mux.ServeHTTP)
	t.Setenv("OKTA_TEAM_SYNC_GITHUB_TOKEN", "test-token")

	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	slug, err := client.EnsureTeam(context.Background(), "test-org", "Test Team")
	if err != nil {
		t.Fatalf("EnsureTeam error: %v", err)
	}
	if slug != "test-team" {
		t.Fatalf("expected slug test-team, got %s", slug)
	}
}

func TestTeamExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/test-org/teams/test-team", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		query := r.URL.Query()
		if len(query) != 0 {
			t.Fatalf("unexpected query params: %v", query)
		}
		w.WriteHeader(http.StatusNotFound)
	})

	setupGitHubTestServer(t, mux.ServeHTTP)
	t.Setenv("OKTA_TEAM_SYNC_GITHUB_TOKEN", "test-token")

	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	exists, err := client.TeamExists(context.Background(), "test-org", "test-team")
	if err != nil {
		t.Fatalf("TeamExists error: %v", err)
	}
	if exists {
		t.Fatalf("expected team to be absent")
	}
}

func TestTeamExistsTrue(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/test-org/teams/test-team", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(teamResponse{Slug: "test-team"})
	})

	setupGitHubTestServer(t, mux.ServeHTTP)
	t.Setenv("OKTA_TEAM_SYNC_GITHUB_TOKEN", "test-token")

	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	exists, err := client.TeamExists(context.Background(), "test-org", "test-team")
	if err != nil {
		t.Fatalf("TeamExists error: %v", err)
	}
	if !exists {
		t.Fatalf("expected team to exist")
	}
}

func TestGroupMappingsCRUD(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/test-org/teams/test-team/team-sync/group-mappings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"groups": []Mapping{{GroupID: "g1"}}})
		case http.MethodPatch:
			var payload map[string][]Mapping
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode mappings body: %v", err)
			}
			if len(payload["groups"]) != 2 {
				t.Fatalf("expected 2 groups, got %d", len(payload["groups"]))
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected method: %s", r.Method)
		}
	})
	mux.HandleFunc("/orgs/test-org/teams/test-team", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode permission body: %v", err)
		}
		if payload["permission"] != "maintain" {
			t.Fatalf("unexpected permission payload: %#v", payload)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	setupGitHubTestServer(t, mux.ServeHTTP)
	t.Setenv("OKTA_TEAM_SYNC_GITHUB_TOKEN", "test-token")

	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	mappings, err := client.GetGroupMappings(context.Background(), "test-org", "test-team")
	if err != nil {
		t.Fatalf("GetGroupMappings error: %v", err)
	}
	if len(mappings) != 1 || mappings[0].GroupID != "g1" {
		t.Fatalf("unexpected mappings: %#v", mappings)
	}

	updated := append(mappings, Mapping{GroupID: "g2"})
	if err := client.PatchGroupMappings(context.Background(), "test-org", "test-team", updated); err != nil {
		t.Fatalf("PatchGroupMappings error: %v", err)
	}

	if err := client.SetTeamPermission(context.Background(), "test-org", "test-team", "maintain"); err != nil {
		t.Fatalf("SetTeamPermission error: %v", err)
	}
}

func generatePrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		t.Fatalf("encode private key: %v", err)
	}
	return buf.String()
}

func TestClient_ResolveTokenPerOrg(t *testing.T) {
	privateKey := generatePrivateKeyPEM(t)

	var (
		mu           sync.Mutex
		tokenUsage   = make(map[string][]string)
		tokenReqs    = make(map[string]int)
		expectedResp = teamResponse{Slug: "test-team", ID: 1, Name: "Test Team"}
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/1/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokenReqs["1"]++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "token-org-a",
			"expires_at": time.Now().Add(10 * time.Minute),
		})
	})
	mux.HandleFunc("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokenReqs["2"]++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "token-org-b",
			"expires_at": time.Now().Add(10 * time.Minute),
		})
	})

	recordOrgAuth := func(org string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected method for %s: %s", org, r.Method)
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				t.Fatalf("missing bearer token for %s: %q", org, auth)
			}
			token := strings.TrimPrefix(auth, "Bearer ")

			mu.Lock()
			tokenUsage[org] = append(tokenUsage[org], token)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(expectedResp)
		}
	}

	mux.HandleFunc("/orgs/org-a/teams/test-team", recordOrgAuth("org-a"))
	mux.HandleFunc("/orgs/org-b/teams/test-team", recordOrgAuth("org-b"))
	mux.HandleFunc("/orgs/org-c/teams/test-team", recordOrgAuth("org-c"))
	mux.HandleFunc("/app/installations/3/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokenReqs["3"]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "token-default",
			"expires_at": time.Now().Add(10 * time.Minute),
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("OKTA_TEAM_SYNC_GH_APP_ID", "12345")
	t.Setenv("OKTA_TEAM_SYNC_GH_INSTALLATION_IDS", "org-a:1,org-b:2")
	t.Setenv("OKTA_TEAM_SYNC_GH_INSTALLATION_ID", "3")
	t.Setenv("OKTA_TEAM_SYNC_GH_PRIVATE_KEY", privateKey)
	t.Setenv("OKTA_TEAM_SYNC_GITHUB_TOKEN", "pat-token")

	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	for _, org := range []string{"org-a", "org-b", "org-c"} {
		if _, err := client.EnsureTeam(context.Background(), org, "Test Team"); err != nil {
			t.Fatalf("EnsureTeam(%s) error: %v", org, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(tokenUsage["org-a"]) == 0 || tokenUsage["org-a"][0] != "pat-token" {
		t.Fatalf("expected org-a token pat-token, got %v", tokenUsage["org-a"])
	}
	if len(tokenUsage["org-b"]) == 0 || tokenUsage["org-b"][0] != "pat-token" {
		t.Fatalf("expected org-b token pat-token, got %v", tokenUsage["org-b"])
	}
	if len(tokenUsage["org-c"]) == 0 || tokenUsage["org-c"][0] != "pat-token" {
		t.Fatalf("expected org-c token pat-token, got %v", tokenUsage["org-c"])
	}

	if tokenReqs["1"] != 0 || tokenReqs["2"] != 0 || tokenReqs["3"] != 0 {
		t.Fatalf("expected PAT path, got app token requests: %+v", tokenReqs)
	}
}

func TestClient_TokenCacheRefreshesPerOrg(t *testing.T) {
	privateKey := generatePrivateKeyPEM(t)

	var (
		mu             sync.Mutex
		tokenCalls     int
		authHeaders    []string
		tokenResponses = []string{"token-first", "token-second"}
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/99/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if tokenCalls >= len(tokenResponses) {
			t.Fatalf("unexpected token request count: %d", tokenCalls)
		}
		token := tokenResponses[tokenCalls]
		tokenCalls++

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      token,
			"expires_at": time.Now().Add(20 * time.Minute),
		})
	})

	mux.HandleFunc("/orgs/org-a/teams/test-team", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		auth := r.Header.Get("Authorization")
		mu.Lock()
		authHeaders = append(authHeaders, auth)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(teamResponse{Slug: "test-team", ID: 1, Name: "Test Team"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("OKTA_TEAM_SYNC_GH_APP_ID", "999")
	t.Setenv("OKTA_TEAM_SYNC_GH_INSTALLATION_IDS", "org-a:99")
	t.Setenv("OKTA_TEAM_SYNC_GH_PRIVATE_KEY", privateKey)
	t.Setenv("OKTA_TEAM_SYNC_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("OKTA_TEAM_SYNC_GH_INSTALLATION_ID", "")

	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := client.EnsureTeam(context.Background(), "org-a", "Test Team"); err != nil {
			t.Fatalf("EnsureTeam call %d failed: %v", i+1, err)
		}
	}

	mu.Lock()
	if tokenCalls != 1 {
		mu.Unlock()
		t.Fatalf("expected single token request before expiry, got %d", tokenCalls)
	}
	authSnapshot := append([]string(nil), authHeaders...)
	mu.Unlock()

	if len(authSnapshot) != 2 {
		t.Fatalf("expected two team requests, got %d", len(authSnapshot))
	}
	for idx, header := range authSnapshot {
		if header != "Bearer token-first" {
			t.Fatalf("expected token-first for request %d, got %s", idx+1, header)
		}
	}

	client.auth.app.cacheMu.Lock()
	entry := client.auth.app.cache["org-a"]
	entry.expiresAt = time.Now().Add(-time.Minute)
	client.auth.app.cache["org-a"] = entry
	client.auth.app.cacheMu.Unlock()

	if _, err := client.EnsureTeam(context.Background(), "org-a", "Test Team"); err != nil {
		t.Fatalf("EnsureTeam after expiry failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if tokenCalls != 2 {
		t.Fatalf("expected token refresh after expiry, got %d requests", tokenCalls)
	}
	if authHeaders[len(authHeaders)-1] != "Bearer token-second" {
		t.Fatalf("expected refreshed token-second, got %s", authHeaders[len(authHeaders)-1])
	}
}

func TestParseInstallationIDs_AllowsNewlines(t *testing.T) {
	raw := "org-a:1\norg-b:2\r\norg-c:3,org-d:4"
	parsed, err := parseInstallationIDs(raw)
	if err != nil {
		t.Fatalf("parseInstallationIDs error: %v", err)
	}
	expected := map[string]int64{
		"org-a": 1,
		"org-b": 2,
		"org-c": 3,
		"org-d": 4,
	}
	if len(parsed) != len(expected) {
		t.Fatalf("unexpected count: got %d want %d", len(parsed), len(expected))
	}
	for org, id := range expected {
		if parsed[org] != id {
			t.Fatalf("unexpected id for %s: got %d want %d", org, parsed[org], id)
		}
	}
}
