package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
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

const apiVersionHeader = "2022-11-28"

type tokenCacheEntry struct {
	token     string
	expiresAt time.Time
}

type appAuthConfig struct {
	appID               int64
	privateKey          *rsa.PrivateKey
	installations       map[string]int64
	defaultInstallation int64
	cache               map[string]tokenCacheEntry
	cacheMu             sync.Mutex
}

type authConfig struct {
	pat string
	app *appAuthConfig
}

// Client wraps interactions with the GitHub REST API.
type Client struct {
	BaseURL    string
	httpClient *http.Client
	auth       authConfig
}

// NewClient constructs a GitHub client, resolving authentication via GitHub App and/or PAT.
func NewClient(ctx context.Context) (*Client, error) {
	baseURL := strings.TrimRight(os.Getenv("GITHUB_API_URL"), "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}

	httpClient := &http.Client{Timeout: 120 * time.Second}
	auth, err := resolveAuthConfig()
	if err != nil {
		return nil, err
	}

	return &Client{BaseURL: baseURL, httpClient: httpClient, auth: auth}, nil
}

func (c *Client) newRequest(ctx context.Context, method, path, org string, body any) (*http.Request, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	url := c.BaseURL + path
	req, err := util.BuildJSONRequest(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	token, err := c.tokenForOrg(ctx, org)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", apiVersionHeader)

	return req, nil
}

func (c *Client) doJSON(ctx context.Context, method, path, org string, body any, out any) error {
	req, err := c.newRequest(ctx, method, path, org, body)
	if err != nil {
		return err
	}

	return util.DoJSON(ctx, c.httpClient, req, out)
}

func (c *Client) doRaw(ctx context.Context, method, path, org string, body any) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, org, body)
	if err != nil {
		return nil, err
	}
	return util.DoRaw(ctx, c.httpClient, req)
}

func buildAppJWT(privateKey *rsa.PrivateKey, appID int64) (string, error) {
	now := time.Now().UTC()
	claims := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(appID, 10),
		IssuedAt:  jwt.NewNumericDate(now.Add(-1 * time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(privateKey)
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	return signed, nil
}

func resolveAuthConfig() (authConfig, error) {
	cfg := authConfig{}

	if pat := envFirst("OKTA_TEAM_SYNC_GITHUB_TOKEN", "GITHUB_TOKEN"); pat != "" {
		cfg.pat = pat
		return cfg, nil
	}

	appIDRaw := envFirst("OKTA_TEAM_SYNC_GH_APP_ID", "GH_APP_ID")
	privateKeyRaw := envFirst("OKTA_TEAM_SYNC_GH_PRIVATE_KEY", "GH_PRIVATE_KEY")
	installationIDsRaw := envFirst("OKTA_TEAM_SYNC_GH_INSTALLATION_IDS", "GH_INSTALLATION_IDS")
	defaultInstallationRaw := envFirst("OKTA_TEAM_SYNC_GH_INSTALLATION_ID", "GH_INSTALLATION_ID")

	if appIDRaw != "" || privateKeyRaw != "" || installationIDsRaw != "" || defaultInstallationRaw != "" {
		if appIDRaw == "" || privateKeyRaw == "" {
			return cfg, errors.New("GitHub App authentication requires OKTA_TEAM_SYNC_GH_APP_ID (or GH_APP_ID) and OKTA_TEAM_SYNC_GH_PRIVATE_KEY (or GH_PRIVATE_KEY)")
		}

		appID, err := strconv.ParseInt(appIDRaw, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("parse OKTA_TEAM_SYNC_GH_APP_ID: %w", err)
		}

		privateKey, err := parseGitHubPrivateKey(privateKeyRaw)
		if err != nil {
			return cfg, err
		}

		installations, err := parseInstallationIDs(installationIDsRaw)
		if err != nil {
			return cfg, err
		}

		var defaultInstallation int64
		if defaultInstallationRaw != "" {
			defaultInstallation, err = strconv.ParseInt(defaultInstallationRaw, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("parse OKTA_TEAM_SYNC_GH_INSTALLATION_ID: %w", err)
			}
		}

		if len(installations) == 0 && defaultInstallation == 0 && cfg.pat == "" {
			return cfg, errors.New("no GitHub App installation configured; set OKTA_TEAM_SYNC_GH_INSTALLATION_IDS (or GH_INSTALLATION_IDS), OKTA_TEAM_SYNC_GH_INSTALLATION_ID (or GH_INSTALLATION_ID), or provide OKTA_TEAM_SYNC_GITHUB_TOKEN")
		}

		cfg.app = &appAuthConfig{
			appID:               appID,
			privateKey:          privateKey,
			installations:       installations,
			defaultInstallation: defaultInstallation,
			cache:               make(map[string]tokenCacheEntry),
		}
	}

	if cfg.app == nil && cfg.pat == "" {
		return cfg, errors.New("missing GitHub authentication configuration; set OKTA_TEAM_SYNC_GITHUB_TOKEN (or GITHUB_TOKEN) or configure GitHub App credentials")
	}

	return cfg, nil
}

func parseInstallationIDs(raw string) (map[string]int64, error) {
	result := make(map[string]int64)
	if strings.TrimSpace(raw) == "" {
		return result, nil
	}

	pairs := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.Split(pair, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid OKTA_TEAM_SYNC_GH_INSTALLATION_IDS entry: %q", pair)
		}
		org := strings.ToLower(strings.TrimSpace(parts[0]))
		if org == "" {
			return nil, fmt.Errorf("invalid OKTA_TEAM_SYNC_GH_INSTALLATION_IDS entry: %q", pair)
		}
		id, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse installation id for %q: %w", org, err)
		}
		result[org] = id
	}

	return result, nil
}

func parseGitHubPrivateKey(raw string) (*rsa.PrivateKey, error) {
	keyPEM := strings.ReplaceAll(raw, "\\n", "\n")
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, errors.New("invalid OKTA_TEAM_SYNC_GH_PRIVATE_KEY PEM data")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("OKTA_TEAM_SYNC_GH_PRIVATE_KEY is not an RSA key")
	}
	return rsaKey, nil
}

func (c *Client) tokenForOrg(ctx context.Context, org string) (string, error) {
	if c.auth.app != nil {
		cacheKey := "__default__"
		if org != "" {
			cacheKey = strings.ToLower(org)
		}

		if org != "" {
			if id, ok := c.auth.app.installations[strings.ToLower(org)]; ok {
				return c.auth.app.getInstallationToken(ctx, c.httpClient, c.BaseURL, cacheKey, id)
			}
		}

		if c.auth.app.defaultInstallation != 0 {
			return c.auth.app.getInstallationToken(ctx, c.httpClient, c.BaseURL, cacheKey, c.auth.app.defaultInstallation)
		}

		if c.auth.pat == "" {
			return "", fmt.Errorf("no GitHub App installation configured for org %q and no PAT fallback provided", org)
		}
	}

	if c.auth.pat != "" {
		return c.auth.pat, nil
	}

	return "", errors.New("missing GitHub authentication configuration")
}

func (a *appAuthConfig) getInstallationToken(ctx context.Context, httpClient *http.Client, baseURL, cacheKey string, installationID int64) (string, error) {
	a.cacheMu.Lock()
	if entry, ok := a.cache[cacheKey]; ok && time.Until(entry.expiresAt) > 2*time.Minute {
		token := entry.token
		a.cacheMu.Unlock()
		return token, nil
	}
	a.cacheMu.Unlock()

	jwtToken, err := buildAppJWT(a.privateKey, a.appID)
	if err != nil {
		return "", err
	}

	token, expiresAt, err := requestInstallationToken(ctx, httpClient, baseURL, installationID, jwtToken)
	if err != nil {
		return "", err
	}

	a.cacheMu.Lock()
	a.cache[cacheKey] = tokenCacheEntry{token: token, expiresAt: expiresAt}
	a.cacheMu.Unlock()

	return token, nil
}

func requestInstallationToken(ctx context.Context, httpClient *http.Client, baseURL string, installationID int64, jwtToken string) (string, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", baseURL, installationID)
	req, err := util.BuildJSONRequest(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("X-GitHub-Api-Version", apiVersionHeader)

	var respBody struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}

	if err := util.DoJSON(ctx, httpClient, req, &respBody); err != nil {
		return "", time.Time{}, fmt.Errorf("create installation token: %w", err)
	}

	if respBody.Token == "" {
		return "", time.Time{}, errors.New("received empty installation token from GitHub")
	}

	return respBody.Token, respBody.ExpiresAt, nil
}
