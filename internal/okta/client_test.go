package okta

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testPrivateKey = "" +
	"-----BEGIN PRIVATE KEY-----\n" +
	"MIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQCsVDlz2dCy+QJK\n" +
	"3g6/2YZyOMm6OYvmdOwTX2FMo0TZ4qesdKEWUrMwLRQZvUE/FZvrpFycoWrQov4m\n" +
	"IkyO+qlPj3IbetWleknOB9V+CDDmsR6yWLdp5HTCWpybYqQK/VSJUPjmVgt8IFAG\n" +
	"SR1Tv+kITBZ2a9nuFd2OI10cvN3RkFs4/2AEyFe42bpu7EHv4iWL5XMdhvXg8DH+\n" +
	"fJoUTcnBrlHCMb12u+vnINnY1YXxu/pZrF3KmKzlXleVrZXJlBLPxllfFVJr/vgD\n" +
	"/wcoSK0PsjCF/pZGZYxKHSozEIt90IHN9IDXP5a0nARfB7CIZSgqYr+cSfmStH3g\n" +
	"wMhZpl6NAgMBAAECggEADQ0t8r++5ickzM3HmTEc1R7G7HM6TMBzNr5lDJxa9ROM\n" +
	"9ms43gtyZcYsPQzP2brFvdGLcBNrlxSZIgM8ACIs24k2L62ca7V4zIFcYni1V2t3\n" +
	"szMz5PG4BBY/wSb13J02H1ZCG5PNt99soCU+ct7Yg9fbZamibj06s+6quSf2ts3T\n" +
	"umBL02a1YoFS8RaVquFn4SWcs690gqgLcQbJDe6fF20/GfJST+dLue2EIDWWTgee\n" +
	"vTmoNiGfx8d3y2xJ73TLk4S4GznoFbT7JkHjB4g+BKKIoOt0u4w65pgpUjolvSr0\n" +
	"Di9/STODOGb5CiFODvJpYS0O+XlvWRdzfd/2ORQEUQKBgQDbZiFENYiyMVKryVSy\n" +
	"9SPmAhxIDx6Bkcr0R78kXOc4ee2Hz6LL/ye0PPkZDZLcipEB7KZ8T8zkI3vBEndE\n" +
	"dGTsqXUbYlkHewH1Dt8zaDZkbjn6ufqtRwT/irTqxzZG3K/Iusl6MuCiHj3sYigV\n" +
	"tyLWs+Dy3C2j8I4csIBPGvWy+QKBgQDJE+DovReH9w0OQ7lnweBTrRciRd7WjoEe\n" +
	"nAmbviyEVQQ/3gJVE1WfZjdt+e0Yd4wZ1GM8zon6JZhxYhHpeIcppAUVPp+0DGXZ\n" +
	"JHg9gxgOxKr5P56hvpK8hD7p3JMeuu6UPflOwvdXnmCziYfWbUeP6uSCbU8sHOmR\n" +
	"D3o1Xj4ZNQKBgGfEAe/UsfY1Rbhh3GFXd8cNMHsUS4VUgvzOAiUcm28mm6UkGwcI\n" +
	"gqrIO7gRp2gPUU3rs0IQLAOqlJlYNnh15FXaP7zX4uuaze4tPnt9ylvtlhZzZ5AU\n" +
	"itShsbdoyM7zCWCSlz/oWD3Ut8zZD8RVfXC2WqoCYMOsvknrYIQJaDNhAoGAFTjw\n" +
	"4v+aLTKJATlqpyXSTGKXb3maZGDUBewII5T10925PhhrfJk2z0UVkpjvSkbL1aoR\n" +
	"80gFTg6LwWPNaivbcCyskKp0ZqdsVHfB7RQaBO0C8p2hW4bmq9j1Xu9146dtKN5F\n" +
	"oud2/Ztsr2ZTnPEZvXnYNl/dHFXM3Q+aIWnZ+gUCgYBTea2Dd9Wr6tAXkiFHhz+x\n" +
	"rpWtHijt4ujpNhGUpMhfxxicFFe67xwPv1RRMB8IOeXfVMjsXpyh+mGcMJjMccge\n" +
	"e6MaCt/vMBFahAQyGPFoZ5grogGscYb9DX5nx8H6KToS1qKhqXWUR+wgN4sxQwTO\n" +
	"LKfXXV5b96JgIUnr6tuh/w==\n" +
	"-----END PRIVATE KEY-----\n"

func setupOktaTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/v1/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method for token endpoint: %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Fatalf("unexpected content type: %s", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm error: %v", err)
		}
		if got := r.Form.Get("client_assertion_type"); got != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
			t.Fatalf("unexpected client_assertion_type: %s", got)
		}
		if got := r.Form.Get("client_id"); got != "test-client" {
			t.Fatalf("unexpected client_id: %s", got)
		}
		if got := r.Form.Get("scope"); got != "okta.groups.read okta.logs.read" {
			t.Fatalf("unexpected scope: %s", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	mux.HandleFunc("/", handler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	t.Setenv("OKTA_TEAM_SYNC_OKTA_BASE_URL", server.URL)
	t.Setenv("OKTA_TEAM_SYNC_OKTA_CLIENT_ID", "test-client")
	t.Setenv("OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY", testPrivateKey)
	t.Setenv("OKTA_TEAM_SYNC_OKTA_SCOPES", "okta.groups.read okta.logs.read")

	return server
}

func TestRequestTokenSetsKeyID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/v1/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm error: %v", err)
		}

		assertion := r.Form.Get("client_assertion")
		parts := strings.Split(assertion, ".")
		if len(parts) < 2 {
			t.Fatalf("malformed JWT: %q", assertion)
		}

		headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatalf("decode header: %v", err)
		}
		var header map[string]any
		if err := json.Unmarshal(headerJSON, &header); err != nil {
			t.Fatalf("unmarshal header: %v", err)
		}
		if kid, _ := header["kid"].(string); kid != "test-kid" {
			t.Fatalf("unexpected kid: %v", header["kid"])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	t.Setenv("OKTA_TEAM_SYNC_OKTA_BASE_URL", server.URL)
	t.Setenv("OKTA_TEAM_SYNC_OKTA_CLIENT_ID", "test-client")
	t.Setenv("OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY", testPrivateKey)
	t.Setenv("OKTA_TEAM_SYNC_OKTA_SCOPES", "okta.groups.read okta.logs.read")
	t.Setenv("OKTA_TEAM_SYNC_OKTA_KEY_ID", "test-kid")

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	if _, err := client.tokenSource.Token(context.Background()); err != nil {
		t.Fatalf("Token error: %v", err)
	}
}

func TestListGroupsByPrefix(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/groups", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-access-token" {
			t.Fatalf("unexpected authorization header: %s", got)
		}
		if r.URL.Query().Get("after") == "" && !strings.Contains(r.URL.RawQuery, "q=Okta-Team-") {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}

		switch r.URL.Query().Get("after") {
		case "":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Link", "<http://"+r.Host+"/api/v1/groups?after=token>; rel=\"next\"")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id":      "g1",
					"profile": map[string]string{"name": "Okta-Team-Platform", "description": "Platform"},
				},
				{
					"id":      "g2",
					"profile": map[string]string{"name": "Other-Group", "description": "Ignored"},
				},
			})
		case "token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id":      "g3",
					"profile": map[string]string{"name": "Okta-Team-SRE", "description": "SRE"},
				},
			})
		default:
			t.Fatalf("unexpected page token: %s", r.URL.Query().Get("after"))
		}
	})

	setupOktaTestServer(t, mux.ServeHTTP)

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	groups, err := client.ListGroupsByPrefix(context.Background(), "Okta-Team-")
	if err != nil {
		t.Fatalf("ListGroupsByPrefix error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].ID != "g1" || groups[1].ID != "g3" {
		t.Fatalf("unexpected groups: %#v", groups)
	}
}

func TestFetchSystemLogDelta(t *testing.T) {
	mux := http.NewServeMux()
	var calls atomic.Int32
	mux.HandleFunc("/api/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-access-token" {
			t.Errorf("unexpected authorization header: %s", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			if !strings.Contains(r.URL.RawQuery, "since=") {
				t.Errorf("expected since in query: %s", r.URL.RawQuery)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Link", "<http://"+r.Host+"/api/v1/logs?cursor=next>; rel=\"next\"")
			_ = json.NewEncoder(w).Encode([]Event{{EventType: "group.user_membership"}})
		case 2:
			if !strings.Contains(r.URL.RawQuery, "cursor=next") {
				t.Errorf("expected cursor in query: %s", r.URL.RawQuery)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]Event{})
		default:
			t.Errorf("unexpected extra request: %s", r.URL.String())
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})

	setupOktaTestServer(t, mux.ServeHTTP)

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	events, cursor, err := client.FetchSystemLogDelta(context.Background(), nil, 5*time.Minute)
	if err != nil {
		t.Fatalf("FetchSystemLogDelta error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if cursor == "" {
		t.Fatalf("expected next cursor")
	}
}

func TestFetchSystemLogDeltaWithCursor(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-access-token" {
			t.Fatalf("unexpected authorization header: %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Event{})
	})

	server := setupOktaTestServer(t, mux.ServeHTTP)

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	cursorURL := server.URL + "/next"
	events, cursor, err := client.FetchSystemLogDelta(context.Background(), &cursorURL, 5*time.Minute)
	if err != nil {
		t.Fatalf("FetchSystemLogDelta error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events, got %d", len(events))
	}
	if cursor != "" {
		t.Fatalf("expected empty cursor, got %s", cursor)
	}
}

func TestFetchSystemLogDeltaUnavailable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limit"}`))
	})

	setupOktaTestServer(t, mux.ServeHTTP)

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	_, _, err = client.FetchSystemLogDelta(context.Background(), nil, time.Minute)
	var unavailable *SystemLogUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected SystemLogUnavailableError, got %v", err)
	}
	if !errors.Is(err, ErrSystemLogUnavailable) {
		t.Fatalf("expected ErrSystemLogUnavailable unwrap, got %v", err)
	}
	if unavailable.Status != http.StatusTooManyRequests {
		t.Fatalf("unexpected status: %d", unavailable.Status)
	}
}
