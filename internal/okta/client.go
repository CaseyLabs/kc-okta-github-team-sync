package okta

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/CaseyLabs/okta-github-team-sync/internal/util"
)

func envFirst(keys ...string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val
		}
	}
	return ""
}

// Client wraps the Okta REST API.
type Client struct {
	BaseURL     string
	httpClient  *http.Client
	tokenSource *clientCredentialsSource
}

// SystemLogUnavailableError indicates that Okta system-log events could not be retrieved.
type SystemLogUnavailableError struct {
	Status int
	URL    string
	Body   string
}

// ErrSystemLogUnavailable is returned when the Okta system log cannot be retrieved and a fallback is required.
var ErrSystemLogUnavailable = errors.New("okta system log unavailable")

func (e *SystemLogUnavailableError) Error() string {
	return fmt.Sprintf("okta system log unavailable: status %d", e.Status)
}

func (e *SystemLogUnavailableError) Unwrap() error {
	return ErrSystemLogUnavailable
}

// NewClient constructs an Okta client using environment configuration.
func NewClient() (*Client, error) {
	baseURL := strings.TrimRight(envFirst("OKTA_TEAM_SYNC_OKTA_BASE_URL", "OKTA_BASE_URL"), "/")
	if baseURL == "" {
		return nil, errors.New("OKTA_TEAM_SYNC_OKTA_BASE_URL is not configured")
	}

	clientID := envFirst("OKTA_TEAM_SYNC_OKTA_CLIENT_ID", "OKTA_OAUTH_CLIENT_ID")
	if clientID == "" {
		return nil, errors.New("OKTA_TEAM_SYNC_OKTA_CLIENT_ID is not configured")
	}

	privateKey := envFirst("OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY", "OKTA_OAUTH_PRIVATE_KEY")
	if privateKey == "" {
		return nil, errors.New("OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY is not configured")
	}
	privateKey = strings.ReplaceAll(privateKey, "\\n", "\n")
	keyID := envFirst("OKTA_TEAM_SYNC_OKTA_KEY_ID", "OKTA_OAUTH_KEY_ID")

	tokenURL := envFirst("OKTA_TEAM_SYNC_OKTA_TOKEN_URL", "OKTA_OAUTH_TOKEN_URL")
	if tokenURL == "" {
		tokenURL = baseURL + "/oauth2/v1/token"
	}

	scopes := envFirst("OKTA_TEAM_SYNC_OKTA_SCOPES", "OKTA_OAUTH_SCOPES")
	if scopes == "" {
		scopes = "okta.groups.read okta.logs.read okta.apps.manage"
	}

	rsaKey, err := parseRSAPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parse OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY: %w", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	tokenSource := &clientCredentialsSource{
		clientID:   clientID,
		tokenURL:   tokenURL,
		scopes:     scopes,
		privateKey: rsaKey,
		keyID:      keyID,
		httpClient: httpClient,
	}

	return &Client{
		BaseURL:     baseURL,
		httpClient:  httpClient,
		tokenSource: tokenSource,
	}, nil
}

func (c *Client) newRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return nil, err
	}

	req, err := util.BuildJSONRequest(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	return req, nil
}

// ListGroupsByPrefix returns all groups whose names start with the provided prefix.
func (c *Client) ListGroupsByPrefix(ctx context.Context, prefix string) ([]Group, error) {
	if prefix == "" {
		return nil, errors.New("group prefix must be provided")
	}

	nextURL := fmt.Sprintf("%s/api/v1/groups?q=%s", c.BaseURL, url.QueryEscape(prefix))
	var out []Group
	seen := make(map[string]struct{})

	for nextURL != "" {
		req, err := c.newRequest(ctx, http.MethodGet, nextURL)
		if err != nil {
			return nil, err
		}

		resp, err := util.DoRaw(ctx, c.httpClient, req)
		if err != nil {
			return nil, fmt.Errorf("list Okta groups: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read groups response: %w", err)
		}

		if util.HTTPDebugEnabled() {
			util.LogHTTPResponseBody(req.Method, nextURL, resp.StatusCode, body)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &util.HTTPError{StatusCode: resp.StatusCode, Body: truncateBody(body, 4096), URL: nextURL}
		}

		var payload []struct {
			ID      string `json:"id"`
			Profile struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"profile"`
		}

		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode groups response: %w", err)
		}

		for _, g := range payload {
			if !strings.HasPrefix(g.Profile.Name, prefix) {
				continue
			}
			if _, ok := seen[g.ID]; ok {
				continue
			}
			seen[g.ID] = struct{}{}
			out = append(out, Group{ID: g.ID, Name: g.Profile.Name, Description: g.Profile.Description})
		}

		nextURL = parseLinkHeader(resp.Header.Get("Link"))
	}

	return out, nil
}

// ListGroupMembers returns the users assigned to the specified Okta group.
func (c *Client) ListGroupMembers(ctx context.Context, groupID string) ([]GroupMember, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil, errors.New("group ID must be provided")
	}

	nextURL := fmt.Sprintf("%s/api/v1/groups/%s/users", c.BaseURL, url.PathEscape(groupID))
	var members []GroupMember

	for nextURL != "" {
		req, err := c.newRequest(ctx, http.MethodGet, nextURL)
		if err != nil {
			return nil, err
		}

		resp, err := util.DoRaw(ctx, c.httpClient, req)
		if err != nil {
			return nil, fmt.Errorf("list group members for %s: %w", groupID, err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read group members response: %w", err)
		}

		if util.HTTPDebugEnabled() {
			util.LogHTTPResponseBody(req.Method, nextURL, resp.StatusCode, body)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("list group members for %s: %w", groupID, &util.HTTPError{
				StatusCode: resp.StatusCode,
				Body:       truncateBody(body, 4096),
				URL:        nextURL,
			})
		}

		var payload []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Profile struct {
				Login string `json:"login"`
				Email string `json:"email"`
			} `json:"profile"`
		}

		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode group members for %s: %w", groupID, err)
		}

		for _, m := range payload {
			members = append(members, GroupMember{
				ID:     m.ID,
				Status: m.Status,
				Login:  strings.TrimSpace(m.Profile.Login),
				Email:  strings.TrimSpace(m.Profile.Email),
			})
		}

		nextURL = parseLinkHeader(resp.Header.Get("Link"))
	}

	return members, nil
}

// AssignGroupToApp ensures the Okta group is assigned to the specified application.
func (c *Client) AssignGroupToApp(ctx context.Context, appID, groupID string) error {
	appID = strings.TrimSpace(appID)
	groupID = strings.TrimSpace(groupID)

	if appID == "" {
		return errors.New("app ID must be provided")
	}
	if groupID == "" {
		return errors.New("group ID must be provided")
	}

	endpoint := fmt.Sprintf("%s/api/v1/apps/%s/groups/%s", c.BaseURL, url.PathEscape(appID), url.PathEscape(groupID))

	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return err
	}

	reqBody := map[string]any{}
	req, err := util.BuildJSONRequest(ctx, http.MethodPut, endpoint, reqBody)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := util.DoRaw(ctx, c.httpClient, req)
	if err != nil {
		return fmt.Errorf("assign Okta group to app: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read assign group response: %w", err)
	}

	if util.HTTPDebugEnabled() {
		util.LogHTTPResponseBody(req.Method, endpoint, resp.StatusCode, body)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &util.HTTPError{StatusCode: resp.StatusCode, Body: truncateBody(body, 4096), URL: endpoint}
	}

	return nil
}

// FetchSystemLogDelta returns relevant events and the cursor for the next page.
func (c *Client) FetchSystemLogDelta(ctx context.Context, cursor *string, lookback time.Duration) ([]Event, string, error) {
	var nextURL string

	if cursor != nil && strings.TrimSpace(*cursor) != "" {
		nextURL = *cursor
	} else {
		nextURL = buildSystemLogURL(c.BaseURL, time.Now().Add(-lookback))
	}

	var (
		events     []Event
		nextCursor string
	)

	for nextURL != "" {
		req, err := c.newRequest(ctx, http.MethodGet, nextURL)
		if err != nil {
			return nil, "", err
		}

		resp, err := util.DoRaw(ctx, c.httpClient, req)
		if err != nil {
			var httpErr *util.HTTPError
			if errors.As(err, &httpErr) && shouldFallback(httpErr.StatusCode) {
				return nil, "", &SystemLogUnavailableError{Status: httpErr.StatusCode, URL: httpErr.URL, Body: httpErr.Body}
			}
			return nil, "", fmt.Errorf("fetch Okta system log: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read system log response: %w", err)
		}

		if util.HTTPDebugEnabled() {
			util.LogHTTPResponseBody(req.Method, nextURL, resp.StatusCode, body)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			bodyStr := truncateBody(body, 4096)
			if shouldFallback(resp.StatusCode) {
				return nil, "", &SystemLogUnavailableError{Status: resp.StatusCode, URL: nextURL, Body: bodyStr}
			}
			return nil, "", &util.HTTPError{StatusCode: resp.StatusCode, Body: bodyStr, URL: nextURL}
		}

		var page []Event
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, "", fmt.Errorf("decode system log response: %w", err)
		}
		events = append(events, page...)

		nextCursor = parseLinkHeader(resp.Header.Get("Link"))
		if nextCursor == "" {
			break
		}
		nextURL = nextCursor
	}

	if nextCursor == "" && len(events) > 0 {
		maxPublished := events[0].Published
		for _, event := range events[1:] {
			if event.Published.After(maxPublished) {
				maxPublished = event.Published
			}
		}
		nextCursor = buildSystemLogURL(c.BaseURL, maxPublished)
	}

	return events, nextCursor, nil
}

func buildSystemLogURL(baseURL string, since time.Time) string {
	query := url.Values{}
	query.Set("since", since.UTC().Format(time.RFC3339))
	query.Set("limit", "200")
	query.Set("filter", `eventType co "group.user_membership" or eventType eq "group.lifecycle.create"`)
	return fmt.Sprintf("%s/api/v1/logs?%s", strings.TrimRight(baseURL, "/"), query.Encode())
}

func parseLinkHeader(header string) string {
	if header == "" {
		return ""
	}

	parts := strings.Split(header, ",")
	for _, part := range parts {
		section := strings.TrimSpace(part)
		if !strings.Contains(section, "rel=\"next\"") {
			continue
		}

		subparts := strings.Split(section, ";")
		if len(subparts) == 0 {
			continue
		}

		link := strings.TrimSpace(subparts[0])
		link = strings.Trim(link, "<>")
		return link
	}

	return ""
}

func shouldFallback(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusNotFound:
		return true
	}
	if status >= 500 && status <= 599 {
		return true
	}
	return false
}

type clientCredentialsSource struct {
	clientID   string
	tokenURL   string
	scopes     string
	privateKey *rsa.PrivateKey
	keyID      string
	httpClient *http.Client
	mu         sync.Mutex
	token      string
	expiresAt  time.Time
}

func (s *clientCredentialsSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && time.Until(s.expiresAt) > time.Minute {
		return s.token, nil
	}

	token, expiry, err := s.requestToken(ctx)
	if err != nil {
		return "", err
	}

	s.token = token
	s.expiresAt = expiry
	return token, nil
}

func (s *clientCredentialsSource) requestToken(ctx context.Context) (string, time.Time, error) {
	jti, err := generateJTI()
	if err != nil {
		return "", time.Time{}, err
	}

	now := time.Now().UTC()
	claims := jwt.RegisteredClaims{
		Issuer:    s.clientID,
		Subject:   s.clientID,
		Audience:  jwt.ClaimStrings{s.tokenURL},
		ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(now.Add(-1 * time.Minute)),
		ID:        jti,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if s.keyID != "" {
		token.Header["kid"] = s.keyID
	}

	assertion, err := token.SignedString(s.privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign client assertion: %w", err)
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", s.scopes)
	form.Set("client_id", s.clientID)
	form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	form.Set("client_assertion", assertion)

	payload := []byte(form.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewReader(payload))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gh-team-sync-agent/okta-oauth")
	copyPayload := append([]byte(nil), payload...)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(copyPayload)), nil
	}

	resp, err := util.DoRaw(ctx, s.httpClient, req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("request Okta access token: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read token response: %w", err)
	}

	if util.HTTPDebugEnabled() {
		util.LogHTTPResponseBody(req.Method, s.tokenURL, resp.StatusCode, body)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, &util.HTTPError{StatusCode: resp.StatusCode, Body: truncateBody(body, 4096), URL: s.tokenURL}
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}

	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", time.Time{}, fmt.Errorf("decode token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", time.Time{}, errors.New("token response missing access_token")
	}

	if strings.ToLower(tokenResp.TokenType) != "bearer" {
		return "", time.Time{}, fmt.Errorf("unexpected token type %q", tokenResp.TokenType)
	}

	expiresIn := time.Duration(tokenResp.ExpiresIn) * time.Second
	if tokenResp.ExpiresIn <= 0 {
		expiresIn = 5 * time.Minute
	}

	return tokenResp.AccessToken, now.Add(expiresIn), nil
}

func generateJTI() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate jti: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func parseRSAPrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, errors.New("failed to decode PEM block")
	}

	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("parsed key is not RSA private key")
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", block.Type)
	}
}

func truncateBody(body []byte, limit int) string {
	if limit <= 0 || len(body) <= limit {
		return string(body)
	}
	return string(body[:limit])
}
