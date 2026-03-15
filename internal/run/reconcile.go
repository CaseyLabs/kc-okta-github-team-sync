package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/CaseyLabs/okta-github-team-sync/internal/github"
	"github.com/CaseyLabs/okta-github-team-sync/internal/naming"
	"github.com/CaseyLabs/okta-github-team-sync/internal/okta"
	"github.com/CaseyLabs/okta-github-team-sync/internal/util"
)

// Config captures runtime configuration for the reconciliation workflow.
type Config struct {
	Organizations []string
	GroupPrefix   string
	Lookback      time.Duration
	StatePath     string
	MaxWorkers    int
	DryRun        bool
	OktaAppIDs    []string
}

const groupOperationTimeout = 120 * time.Second

type githubClient interface {
	EnsureTeam(ctx context.Context, org, name string) (string, error)
	TeamExists(ctx context.Context, org, slug string) (bool, error)
	GetGroupMappings(ctx context.Context, org, slug string) ([]github.Mapping, error)
	PatchGroupMappings(ctx context.Context, org, slug string, groups []github.Mapping) error
	SetTeamPermission(ctx context.Context, org, slug, permission string) error
}

type oktaClient interface {
	FetchSystemLogDelta(ctx context.Context, cursor *string, lookback time.Duration) ([]okta.Event, string, error)
	ListGroupsByPrefix(ctx context.Context, prefix string) ([]okta.Group, error)
	AssignGroupToApp(ctx context.Context, appID, groupID string) error
}

// Reconcile performs the Okta→GitHub team synchronization.
func Reconcile(ctx context.Context, gh *github.Client, oktaClient *okta.Client, cfg Config, logger *slog.Logger) error {
	return reconcile(ctx, gh, oktaClient, cfg, logger)
}

func reconcile(ctx context.Context, gh githubClient, oktaClient oktaClient, cfg Config, logger *slog.Logger) error {
	if gh == nil || oktaClient == nil {
		return errors.New("github and okta clients must be provided")
	}

	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}

	if cfg.GroupPrefix == "" {
		return errors.New("group prefix is required")
	}

	if len(cfg.Organizations) == 0 {
		return errors.New("at least one organization must be provided")
	}

	if cfg.Lookback <= 0 {
		cfg.Lookback = 15 * time.Minute
	}

	if cfg.StatePath == "" {
		cfg.StatePath = DefaultStatePath
	}

	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 2
	}

	cursor, err := LoadCursor(cfg.StatePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	logger.Info("loaded state", "cursor", truncateCursor(cursor))
	logger.Info("fetching Okta system log delta", "lookback", cfg.Lookback, "previous_cursor", truncateCursor(cursor))

	var cursorPtr *string
	if cursor != "" {
		cursorPtr = &cursor
	}

	fallbackAll := false
	events, nextCursor, err := oktaClient.FetchSystemLogDelta(ctx, cursorPtr, cfg.Lookback)
	if err != nil {
		var unavailable *okta.SystemLogUnavailableError
		if errors.As(err, &unavailable) {
			logger.Warn("okta system log unavailable; running full reconciliation", "status", unavailable.Status, "url", unavailable.URL)
			events = nil
			nextCursor = cursor
			fallbackAll = true
		} else {
			var httpErr *util.HTTPError
			if errors.As(err, &httpErr) {
				logger.Error("fetch system log failed", "status", httpErr.StatusCode, "url", httpErr.URL, "error", httpErr.Body)
			}
			return fmt.Errorf("fetch system log: %w", err)
		}
	} else {
		logger.Info("fetched system log events", "count", len(events))
	}

	logger.Info("listing Okta groups", "prefix", cfg.GroupPrefix)
	groups, err := oktaClient.ListGroupsByPrefix(ctx, cfg.GroupPrefix)
	if err != nil {
		var httpErr *util.HTTPError
		if cfg.DryRun && errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden) {
			logger.Warn("unable to list Okta groups; dry-run will not process candidates", "status", httpErr.StatusCode, "url", httpErr.URL, "error", httpErr.Body)
			groups = nil
		} else {
			return fmt.Errorf("list Okta groups: %w", err)
		}
	} else {
		logger.Info("fetched okta groups", "count", len(groups))
	}

	candidates := deriveCandidateGroups(groups, events, cursor, fallbackAll, logger)

	if len(candidates) == 0 {
		logger.Info("no candidate groups to process")
		if cfg.DryRun {
			logger.Info("dry-run: skipping cursor persistence", "previous", truncateCursor(cursor), "next", truncateCursor(nextCursor))
		} else {
			if err := persistCursor(cfg.StatePath, cursor, nextCursor); err != nil {
				return err
			}
		}
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	logger.Info("processing candidates", "count", len(candidates), "dry_run", cfg.DryRun)

	var (
		errMu sync.Mutex
		errs  []error
	)

	progress := newProgressTracker(len(candidates), logger)

	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(cfg.MaxWorkers)

	for _, candidate := range candidates {
		candidate := candidate
		g.Go(func() error {
			progress.Start(candidate)
			err := processGroup(groupCtx, gh, oktaClient, candidate, cfg, logger)
			progress.Done(candidate, err)
			if err != nil {
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
			}
			return nil
		})
	}

	_ = g.Wait()

	if cfg.DryRun {
		logger.Info("dry-run: skipping cursor persistence", "previous", truncateCursor(cursor), "next", truncateCursor(nextCursor))
	}

	if len(errs) > 0 {
		if !cfg.DryRun {
			logger.Error("reconciliation encountered errors; retaining cursor", "previous", truncateCursor(cursor), "next", truncateCursor(nextCursor), "error_count", len(errs))
		}
		return errors.Join(errs...)
	}

	if cfg.DryRun {
		logger.Info("dry-run: skipping cursor persistence", "previous", truncateCursor(cursor), "next", truncateCursor(nextCursor))
	} else {
		if err := persistCursor(cfg.StatePath, cursor, nextCursor); err != nil {
			return err
		}
		logger.Info("cursor advanced", "previous", truncateCursor(cursor), "next", truncateCursor(nextCursor))
	}

	logger.Info("reconciliation complete", "processed", len(candidates))
	return nil
}

func processGroup(ctx context.Context, gh githubClient, okta oktaClient, group okta.Group, cfg Config, logger *slog.Logger) error {
	if cfg.DryRun {
		return processGroupDryRun(ctx, gh, group, cfg, logger)
	}

	teamName, role, err := parseGroupName(group.Name, cfg.GroupPrefix)
	if err != nil {
		logger.Warn("skip group", "group", group.Name, "error", err)
		return nil
	}

	if len(cfg.OktaAppIDs) > 0 {
		for _, appID := range cfg.OktaAppIDs {
			appID = strings.TrimSpace(appID)
			if appID == "" {
				continue
			}
			if err := withTimeout(ctx, groupOperationTimeout, func(opCtx context.Context) error {
				return okta.AssignGroupToApp(opCtx, appID, group.ID)
			}); err != nil {
				logger.Error("assign group to Okta app failed", "group", group.ID, "app", appID, "error", err)
				return fmt.Errorf("assign group %s to app %s: %w", group.ID, appID, err)
			}
			logger.Info("ensured Okta group assignment", "group", group.ID, "app", appID)
		}
	}

	slugCache := make(map[string]string)

	var groupErrs []error

	for _, org := range cfg.Organizations {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		slug, ok := slugCache[org]
		if !ok {
			var slugVal string
			ensureErr := withTimeout(ctx, groupOperationTimeout, func(opCtx context.Context) error {
				var err error
				slugVal, err = gh.EnsureTeam(opCtx, org, teamName)
				return err
			})
			if ensureErr != nil {
				logger.Error("ensure team failed", "org", org, "team", teamName, "error", ensureErr)
				groupErrs = append(groupErrs, fmt.Errorf("ensure team %s in org %s: %w", teamName, org, ensureErr))
				continue
			}
			slug = slugVal
			slugCache[org] = slug
			logger.Info("ensured team", "org", org, "team", teamName, "slug", slug)
		}

		if err := ensureGroupMapping(ctx, gh, org, slug, group, logger); err != nil {
			groupErrs = append(groupErrs, err)
			continue
		}

		if role != "" {
			permission, ok := mapRoleToPermission(role)
			if !ok {
				logger.Warn("unsupported role; skipping permission update", "role", role, "org", org, "team", teamName)
				continue
			}

			if err := withTimeout(ctx, groupOperationTimeout, func(opCtx context.Context) error {
				return gh.SetTeamPermission(opCtx, org, slug, permission)
			}); err != nil {
				logger.Error("set team permission failed", "org", org, "team", teamName, "permission", permission, "error", err)
				groupErrs = append(groupErrs, fmt.Errorf("set permission %s for team %s in org %s: %w", permission, teamName, org, err))
			} else {
				logger.Info("team permission updated", "org", org, "team", teamName, "permission", permission)
			}
		}
	}

	if len(groupErrs) > 0 {
		return errors.Join(groupErrs...)
	}

	return nil
}

func processGroupDryRun(ctx context.Context, gh githubClient, group okta.Group, cfg Config, logger *slog.Logger) error {
	teamName, role, err := parseGroupName(group.Name, cfg.GroupPrefix)
	if err != nil {
		logger.Warn("dry-run: skip group", "group", group.Name, "error", err)
		return nil
	}

	slug := naming.SlugifyTeamName(teamName)

	if len(cfg.OktaAppIDs) > 0 {
		for _, appID := range cfg.OktaAppIDs {
			appID = strings.TrimSpace(appID)
			if appID == "" {
				continue
			}
			logger.Info("dry-run: would assign Okta group to app", "group", group.ID, "app", appID)
		}
	}

	permission := ""
	if role != "" {
		if mapped, ok := mapRoleToPermission(role); ok {
			permission = mapped
		} else {
			logger.Warn("dry-run: unsupported role; would skip permission update", "role", role, "team", teamName)
		}
	}

	for _, org := range cfg.Organizations {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		logger.Info("dry-run: checking team existence", "org", org, "team", teamName, "slug", slug)
		var exists bool
		if err := withTimeout(ctx, groupOperationTimeout, func(opCtx context.Context) error {
			var err error
			exists, err = gh.TeamExists(opCtx, org, slug)
			return err
		}); err != nil {
			logger.Error("dry-run: failed to check team existence", "org", org, "team", teamName, "error", err)
			continue
		}

		if !exists {
			logger.Info("dry-run: would create team", "org", org, "team", teamName, "slug", slug)
			logger.Info("dry-run: would add group mapping", "org", org, "team", slug, "group", group.ID)
			if permission != "" {
				logger.Info("dry-run: would assign permission", "org", org, "team", teamName, "permission", permission)
			}
			continue
		}

		logger.Info("dry-run: team exists", "org", org, "team", teamName, "slug", slug)

		var mappings []github.Mapping
		logger.Info("dry-run: fetching group mappings", "org", org, "team", slug, "group", group.ID)
		start := time.Now()
		waiting := make(chan struct{})
		go func() {
			select {
			case <-waiting:
			case <-time.After(30 * time.Second):
				logger.Warn("dry-run: still waiting for group mappings", "org", org, "team", slug, "group", group.ID, "elapsed", time.Since(start))
			}
		}()
		if err := withTimeout(ctx, groupOperationTimeout, func(opCtx context.Context) error {
			var err error
			mappings, err = gh.GetGroupMappings(opCtx, org, slug)
			return err
		}); err != nil {
			close(waiting)
			logger.Error("dry-run: failed to fetch mappings", "org", org, "team", slug, "error", err)
			continue
		}
		close(waiting)
		logger.Info("dry-run: fetched group mappings", "org", org, "team", slug, "count", len(mappings), "elapsed", time.Since(start))

		newMapping := github.Mapping{
			GroupID:          group.ID,
			GroupName:        group.Name,
			GroupDescription: group.Description,
		}

		if _, added := appendMappingIfMissing(mappings, newMapping); added {
			logger.Info("dry-run: would add group mapping", "org", org, "team", slug, "group", group.ID)
		} else {
			logger.Info("dry-run: mapping already present", "org", org, "team", slug, "group", group.ID)
		}

		if permission != "" {
			logger.Info("dry-run: would assign permission", "org", org, "team", teamName, "permission", permission)
		}
	}

	return nil
}

func ensureGroupMapping(ctx context.Context, gh githubClient, org, slug string, group okta.Group, logger *slog.Logger) error {
	logger.Info("fetching group mappings", "org", org, "team", slug, "group", group.ID)

	var mappings []github.Mapping
	var existing github.Mapping
	exists := false
	if err := withTimeout(ctx, groupOperationTimeout, func(opCtx context.Context) error {
		var err error
		mappings, err = gh.GetGroupMappings(opCtx, org, slug)
		return err
	}); err != nil {
		logger.Error("fetch group mappings failed", "org", org, "team", slug, "error", err)
		return fmt.Errorf("get mappings for %s/%s: %w", org, slug, err)
	}

	newMapping := github.Mapping{
		GroupID:          group.ID,
		GroupName:        group.Name,
		GroupDescription: group.Description,
	}

	updated, added := appendMappingIfMissing(mappings, newMapping)
	if !added {
		for _, mapping := range mappings {
			if mapping.GroupID == group.ID {
				existing = mapping
				exists = true
				break
			}
		}

		if exists && strings.EqualFold(existing.Status, "synced") {
			logger.Info("mapping already exists", "org", org, "team", slug, "group", group.ID)
			return nil
		}

		logger.Info("mapping exists but is not synced; refreshing", "org", org, "team", slug, "group", group.ID, "status", existing.Status)
		updated = replaceMapping(mappings, newMapping)
	}

	logger.Info("updating group mappings", "org", org, "team", slug, "group", group.ID, "existing_count", len(mappings))

	if err := withTimeout(ctx, groupOperationTimeout, func(opCtx context.Context) error {
		return gh.PatchGroupMappings(opCtx, org, slug, updated)
	}); err != nil {
		logger.Error("update group mappings failed", "org", org, "team", slug, "group", group.ID, "error", err)
		return fmt.Errorf("patch mappings for %s/%s: %w", org, slug, err)
	}

	logger.Info("added group mapping", "org", org, "team", slug, "group", group.ID)
	return nil
}

func appendMappingIfMissing(existing []github.Mapping, candidate github.Mapping) ([]github.Mapping, bool) {
	for _, mapping := range existing {
		if mapping.GroupID == candidate.GroupID {
			return existing, false
		}
	}
	return append(existing, candidate), true
}

func replaceMapping(existing []github.Mapping, candidate github.Mapping) []github.Mapping {
	updated := make([]github.Mapping, 0, len(existing))
	for _, mapping := range existing {
		if mapping.GroupID == candidate.GroupID {
			updated = append(updated, candidate)
			continue
		}
		// drop status metadata so we don't persist disabled/synced_at fields when patching
		updated = append(updated, github.Mapping{
			GroupID:          mapping.GroupID,
			GroupName:        mapping.GroupName,
			GroupDescription: mapping.GroupDescription,
		})
	}
	return updated
}

func mapRoleToPermission(role string) (string, bool) {
	switch strings.ToLower(role) {
	case "admin":
		return "admin", true
	case "maintain":
		return "maintain", true
	case "push", "write", "member":
		return "push", true
	case "pull", "read", "viewer", "read-only":
		return "pull", true
	case "triage":
		return "triage", true
	default:
		return "", false
	}
}

func persistCursor(path, previous, next string) error {
	value := next
	if value == "" {
		value = previous
	}
	if err := SaveCursor(path, value); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func truncateCursor(cursor string) string {
	if len(cursor) <= 24 {
		return cursor
	}
	return cursor[:24] + "…"
}

func deriveCandidateGroups(groups []okta.Group, events []okta.Event, previousCursor string, fallbackAll bool, logger *slog.Logger) []okta.Group {
	groupsByID := make(map[string]okta.Group, len(groups))
	for _, group := range groups {
		groupsByID[group.ID] = group
	}

	candidates := make(map[string]okta.Group)

	if fallbackAll || strings.TrimSpace(previousCursor) == "" {
		for _, group := range groups {
			candidates[group.ID] = group
		}
	} else {
		for _, event := range events {
			for _, target := range event.Targets {
				if !strings.EqualFold(target.Type, "group") {
					continue
				}

				if group, ok := groupsByID[target.ID]; ok {
					candidates[group.ID] = group
					continue
				}

				if logger != nil {
					logger.Warn("event references group missing from listing", "group_id", target.ID, "display_name", target.DisplayName)
				}
			}
		}
	}

	result := make([]okta.Group, 0, len(candidates))
	for _, group := range candidates {
		result = append(result, group)
	}

	return result
}

func parseGroupName(name, prefix string) (string, string, error) {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return "", "", errors.New("group name is empty")
	}

	if !strings.HasPrefix(raw, prefix) {
		return "", "", fmt.Errorf("group %q does not start with prefix %q", name, prefix)
	}

	remainder := strings.TrimSpace(raw[len(prefix):])
	if remainder == "" {
		return "", "", errors.New("group name missing after prefix")
	}

	role := ""
	base := remainder
	if idx := strings.LastIndex(remainder, "__"); idx != -1 {
		if candidate := strings.TrimSpace(remainder[idx+2:]); candidate != "" {
			role = candidate
			base = strings.TrimSpace(remainder[:idx])
		}
	} else if idx := strings.LastIndex(remainder, ":"); idx != -1 {
		if candidate := strings.TrimSpace(remainder[idx+1:]); candidate != "" {
			role = candidate
			base = strings.TrimSpace(remainder[:idx])
		}
	}

	if base == "" {
		return "", "", errors.New("derived team name is empty")
	}

	teamName := prefix + base
	return teamName, role, nil
}

// ParseGroupName exposes the group-name parsing logic for diagnostics and tooling.
func ParseGroupName(name, prefix string) (string, string, error) {
	return parseGroupName(name, prefix)
}

type progressTracker struct {
	total     int64
	logger    *slog.Logger
	started   atomic.Int64
	completed atomic.Int64
}

func newProgressTracker(total int, logger *slog.Logger) *progressTracker {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	return &progressTracker{total: int64(total), logger: logger}
}

func (p *progressTracker) Start(group okta.Group) {
	current := p.started.Add(1)
	p.logger.Info("group processing started", "group", group.Name, "current", current, "total", p.total)
}

func (p *progressTracker) Done(group okta.Group, err error) {
	completed := p.completed.Add(1)
	progress := fmt.Sprintf("%d/%d", completed, p.total)
	if err != nil {
		p.logger.Error("group processing finished with error", "group", group.Name, "progress", progress, "error", err)
		return
	}
	p.logger.Info("group processing finished", "group", group.Name, "progress", progress)
}

func withTimeout(ctx context.Context, d time.Duration, fn func(context.Context) error) error {
	if d <= 0 {
		return fn(ctx)
	}
	tctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return fn(tctx)
}
