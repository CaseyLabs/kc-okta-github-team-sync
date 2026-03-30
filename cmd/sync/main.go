package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/CaseyLabs/okta-github-team-sync/internal/github"
	"github.com/CaseyLabs/okta-github-team-sync/internal/okta"
	"github.com/CaseyLabs/okta-github-team-sync/internal/run"
	"github.com/CaseyLabs/okta-github-team-sync/internal/util"
)

var (
	logLevelFlag  = flag.String("log-level", "", "override log level (debug, info, warn, error)")
	httpDebugFlag = flag.Bool("http-debug", false, "emit HTTP request/response debug logs")
)

const groupPrefixEnvVar = "OKTA_TEAM_SYNC_GROUP_PREFIX"

func envFirst(keys ...string) string {
	for _, key := range keys {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val
		}
	}
	return ""
}

func main() {
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logLevel := parseLogLevel(resolveLogLevel(*logLevelFlag, os.Getenv("LOG_LEVEL")))
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	if shouldEnableHTTPDebug(logLevel, *httpDebugFlag) {
		util.EnableHTTPDebug(log.With("component", "http"))
	}

	log.Info("starting okta-team-sync", "log_level", logLevel.String(), "dry_run", os.Getenv("DRY_RUN"))

	log.Info("loading configuration")
	cfg, err := loadConfig()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	log.Info("configuration loaded",
		"orgs", len(cfg.Organizations),
		"group_prefix", cfg.GroupPrefix,
		"lookback_minutes", cfg.Lookback/time.Minute,
		"state_path", cfg.StatePath,
		"max_workers", cfg.MaxWorkers,
		"dry_run", cfg.DryRun)

	log.Info("initializing Okta client")
	oktaClient, err := okta.NewClient()
	if err != nil {
		log.Error("failed to create Okta client", "error", err)
		os.Exit(1)
	}

	log.Info("initializing GitHub client")
	ghClient, err := github.NewClient(ctx)
	if err != nil {
		log.Error("failed to create GitHub client", "error", err)
		os.Exit(1)
	}

	if err := run.Reconcile(ctx, ghClient, oktaClient, cfg, log); err != nil {
		log.Error("reconciliation failed", "error", err)
		os.Exit(1)
	}

	log.Info("synchronization complete")
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info", "":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

func resolveLogLevel(flagValue, envValue string) string {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue != "" {
		return flagValue
	}
	return envValue
}

func shouldEnableHTTPDebug(level slog.Level, flagValue bool) bool {
	if flagValue {
		return true
	}

	raw := strings.TrimSpace(os.Getenv("HTTP_DEBUG"))
	if raw == "" {
		return level <= slog.LevelDebug
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on", "debug":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return level <= slog.LevelDebug
}

func loadConfig() (run.Config, error) {
	if !flag.Parsed() {
		_ = flag.CommandLine.Parse([]string{}) // ensure flags don't interfere when running under go test
	}

	oktaAppIDs := parseOktaAppIDs()

	prefix := strings.TrimSpace(os.Getenv(groupPrefixEnvVar))
	if prefix == "" {
		return run.Config{}, fmt.Errorf("%s must be set", groupPrefixEnvVar)
	}

	orgs := parseOrgList(envFirst("ORG_LIST", "OKTA_TEAM_SYNC_ORG_LIST"))
	if len(orgs) == 0 {
		derived, err := parseInstallationOrgList(envFirst("OKTA_TEAM_SYNC_GH_INSTALLATION_IDS", "GH_INSTALLATION_IDS"))
		if err != nil {
			return run.Config{}, err
		}
		orgs = derived
	}
	if len(orgs) == 0 {
		return run.Config{}, errors.New("provide ORG_LIST or derive organizations via OKTA_TEAM_SYNC_GH_INSTALLATION_IDS")
	}

	lookbackMinutes := 15
	if raw := strings.TrimSpace(os.Getenv("LOOKBACK_MINUTES")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			lookbackMinutes = v
		} else {
			return run.Config{}, fmt.Errorf("invalid LOOKBACK_MINUTES value %q", raw)
		}
	}

	statePath := strings.TrimSpace(os.Getenv("STATE_PATH"))
	if statePath == "" {
		statePath = run.DefaultStatePath
	}

	maxWorkers := 2
	if raw := strings.TrimSpace(os.Getenv("MAX_WORKERS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			maxWorkers = v
		} else {
			return run.Config{}, fmt.Errorf("invalid MAX_WORKERS value %q", raw)
		}
	}

	dryRun := false
	if raw := strings.TrimSpace(os.Getenv("DRY_RUN")); raw != "" {
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "on":
			dryRun = true
		case "0", "false", "no", "off":
			dryRun = false
		default:
			return run.Config{}, fmt.Errorf("invalid DRY_RUN value %q", raw)
		}
	}

	return run.Config{
		Organizations: orgs,
		GroupPrefix:   prefix,
		Lookback:      time.Duration(lookbackMinutes) * time.Minute,
		StatePath:     statePath,
		MaxWorkers:    maxWorkers,
		DryRun:        dryRun,
		OktaAppIDs:    oktaAppIDs,
	}, nil
}

func parseOktaAppIDs() []string {
	var ids []string
	seen := make(map[string]struct{})

	collect := func(raw string) {
		if raw == "" {
			return
		}
		parts := strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == '\n' || r == '\r'
		})
		for _, part := range parts {
			id := strings.TrimSpace(part)
			if hash := strings.IndexRune(id, '#'); hash >= 0 {
				id = strings.TrimSpace(id[:hash])
			}
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	collect(strings.TrimSpace(os.Getenv("OKTA_GITHUB_APP_ID")))
	collect(strings.TrimSpace(os.Getenv("OKTA_GITHUB_APP_IDS")))

	return ids
}

func parseOrgList(raw string) []string {
	var orgs []string
	seen := make(map[string]struct{})

	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
		org := strings.TrimSpace(part)
		if org == "" {
			continue
		}
		orgLower := strings.ToLower(org)
		if _, ok := seen[orgLower]; ok {
			continue
		}
		seen[orgLower] = struct{}{}
		orgs = append(orgs, orgLower)
	}

	return orgs
}

func parseInstallationOrgList(raw string) ([]string, error) {
	var orgs []string
	seen := make(map[string]struct{})

	for _, pair := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
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
		if _, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err != nil {
			return nil, fmt.Errorf("parse installation id for %q: %w", org, err)
		}
		if _, ok := seen[org]; ok {
			continue
		}
		seen[org] = struct{}{}
		orgs = append(orgs, org)
	}

	return orgs, nil
}
