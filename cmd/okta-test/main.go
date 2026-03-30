package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

func envFirst(keys ...string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val
		}
	}
	return ""
}

func main() {
	assertionOnly := flag.Bool("assertion-only", false, "only print the client assertion (skip calling the token endpoint)")
	flag.Parse()

	ctx := context.Background()

	baseURL := strings.TrimRight(envFirst("OKTA_TEAM_SYNC_OKTA_BASE_URL", "OKTA_BASE_URL"), "/")
	clientID := envFirst("OKTA_TEAM_SYNC_OKTA_CLIENT_ID", "OKTA_OAUTH_CLIENT_ID")
	privateKey := envFirst("OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY", "OKTA_OAUTH_PRIVATE_KEY")
	tokenURL := envFirst("OKTA_TEAM_SYNC_OKTA_TOKEN_URL", "OKTA_OAUTH_TOKEN_URL")
	scopes := envFirst("OKTA_TEAM_SYNC_OKTA_SCOPES", "OKTA_OAUTH_SCOPES")

	if baseURL == "" {
		exitErr(errors.New("OKTA_TEAM_SYNC_OKTA_BASE_URL must be set"))
	}
	if clientID == "" {
		exitErr(errors.New("OKTA_TEAM_SYNC_OKTA_CLIENT_ID must be set"))
	}
	if privateKey == "" {
		exitErr(errors.New("OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY must be set"))
	}

	if tokenURL == "" {
		tokenURL = baseURL + "/oauth2/v1/token"
	}
	if scopes == "" {
		scopes = "okta.groups.read okta.logs.read"
	}

	key, err := parseRSAPrivateKey(strings.ReplaceAll(privateKey, "\\n", "\n"))
	if err != nil {
		exitErr(fmt.Errorf("parse OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY: %w", err))
	}

	assertion, err := buildClientAssertion(clientID, tokenURL, key)
	if err != nil {
		exitErr(err)
	}

	fmt.Println("CLIENT_ASSERTION=" + assertion)

	if *assertionOnly {
		return
	}

	resp, err := exchangeForToken(ctx, tokenURL, clientID, assertion, scopes)
	if err != nil {
		exitErr(err)
	}

	fmt.Println("ACCESS_TOKEN=" + resp.AccessToken)
	fmt.Printf("TOKEN_TYPE=%s\n", resp.TokenType)
	if resp.ExpiresIn > 0 {
		fmt.Printf("EXPIRES_IN=%d\n", resp.ExpiresIn)
	}
	if resp.Scope != "" {
		fmt.Printf("SCOPE=%s\n", resp.Scope)
	}
}

func exchangeForToken(ctx context.Context, tokenURL, clientID, assertion, scopes string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", scopes)
	form.Set("client_id", clientID)
	form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	form.Set("client_assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("token endpoint returned %d and failed to read body: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(payload))
	}

	var parsed tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	switch strings.ToLower(parsed.TokenType) {
	case "", "bearer":
	default:
		return nil, fmt.Errorf("unexpected token type %q", parsed.TokenType)
	}

	if parsed.AccessToken == "" {
		return nil, errors.New("token response missing access_token")
	}

	return &parsed, nil
}

func buildClientAssertion(clientID, tokenURL string, key *rsa.PrivateKey) (string, error) {
	now := time.Now().UTC()
	claims := jwt.RegisteredClaims{
		Issuer:    clientID,
		Subject:   clientID,
		Audience:  jwt.ClaimStrings{tokenURL},
		IssuedAt:  jwt.NewNumericDate(now.Add(-1 * time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	}
	jti, err := generateJTI()
	if err != nil {
		return "", err
	}
	claims.ID = jti

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	assertion, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign assertion: %w", err)
	}
	return assertion, nil
}

func parseRSAPrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, errors.New("failed to decode PEM block")
	}

	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := keyAny.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("parsed key is not RSA private key")
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", block.Type)
	}
}

func generateJTI() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate jti: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func exitErr(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
