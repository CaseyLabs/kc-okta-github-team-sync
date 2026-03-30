package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/CaseyLabs/okta-github-team-sync/internal/github"
	"github.com/CaseyLabs/okta-github-team-sync/internal/naming"
	"github.com/CaseyLabs/okta-github-team-sync/internal/okta"
	"github.com/CaseyLabs/okta-github-team-sync/internal/run"
	"github.com/CaseyLabs/okta-github-team-sync/internal/util"
)

const groupPrefixEnvVar = "OKTA_TEAM_SYNC_GROUP_PREFIX"

type groupDiag struct {
	group     okta.Group
	teamName  string
	teamSlug  string
	oktaUsers []okta.GroupMember
}

func main() {
	httpDebug := flag.Bool("http-debug", false, "enable verbose HTTP logging")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *httpDebug {
		util.EnableHTTPDebug(logger)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	orgsEnv := strings.TrimSpace(os.Getenv("ORG_LIST"))
	if orgsEnv == "" {
		exitErr(errors.New("ORG_LIST is not configured"))
	}
	orgs := splitAndClean(orgsEnv)
	if len(orgs) == 0 {
		exitErr(errors.New("ORG_LIST contains no organizations"))
	}

	groupPrefix := strings.TrimSpace(os.Getenv(groupPrefixEnvVar))
	if groupPrefix == "" {
		exitErr(fmt.Errorf("%s is not configured", groupPrefixEnvVar))
	}

	oktaClient, err := okta.NewClient()
	if err != nil {
		exitErr(fmt.Errorf("init Okta client: %w", err))
	}

	groups, err := oktaClient.ListGroupsByPrefix(ctx, groupPrefix)
	if err != nil {
		exitErr(fmt.Errorf("list Okta groups: %w", err))
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Name < groups[j].Name
	})

	diagnostics := make([]groupDiag, 0, len(groups))
	for _, grp := range groups {
		teamName, _, err := run.ParseGroupName(grp.Name, groupPrefix)
		if err != nil {
			logger.Warn("skip group with unparsable name", "group", grp.Name, "error", err)
			continue
		}
		members, err := oktaClient.ListGroupMembers(ctx, grp.ID)
		if err != nil {
			logger.Warn("failed to fetch Okta group members", "group", grp.Name, "error", err)
		}
		diagnostics = append(diagnostics, groupDiag{
			group:     grp,
			teamName:  teamName,
			teamSlug:  naming.SlugifyTeamName(teamName),
			oktaUsers: members,
		})
	}

	ghClient, err := github.NewClient(ctx)
	if err != nil {
		exitErr(fmt.Errorf("init GitHub client: %w", err))
	}

	fmt.Printf("=== Okta Groups (prefix %s) ===\n", groupPrefix)
	for _, diag := range diagnostics {
		fmt.Printf("- %s [%s]: %d Okta member(s)\n", diag.group.Name, diag.group.ID, len(diag.oktaUsers))
		if len(diag.oktaUsers) > 0 {
			var identities []string
			for _, u := range diag.oktaUsers {
				identity := firstNonEmpty(u.Login, u.Email, u.ID)
				identities = append(identities, identity)
			}
			sort.Strings(identities)
			fmt.Printf("    Okta identities: %s\n", strings.Join(identities, ", "))
		}
	}

	fmt.Printf("\n=== GitHub Team Sync Diagnostics ===\n")
	for _, org := range orgs {
		if org == "" {
			continue
		}
		fmt.Printf("Org: %s\n", org)
		for _, diag := range diagnostics {
			printTeamDiagnostics(ctx, ghClient, org, diag)
		}
		fmt.Println()
	}
}

func printTeamDiagnostics(ctx context.Context, gh *github.Client, org string, diag groupDiag) {
	teamDetails, err := gh.GetTeamDetails(ctx, org, diag.teamSlug)
	if err != nil {
		var httpErr *util.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			fmt.Printf("  - Team %s (slug %s): not found\n", diag.teamName, diag.teamSlug)
			return
		}
		fmt.Printf("  - Team %s (slug %s): error fetching team details: %v\n", diag.teamName, diag.teamSlug, err)
		return
	}

	fmt.Printf("  - Team %s (slug %s, id %d)\n", teamDetails.Name, teamDetails.Slug, teamDetails.ID)

	mappings, err := gh.GetGroupMappings(ctx, org, diag.teamSlug)
	if err != nil {
		fmt.Printf("      group mappings: error: %v\n", err)
	} else if len(mappings) == 0 {
		fmt.Printf("      group mappings: none configured\n")
	} else {
		fmt.Printf("      group mappings (%d):\n", len(mappings))
		for _, m := range mappings {
			fmt.Printf("        - %s (%s)\n", m.GroupName, m.GroupID)
		}
	}

	members, err := gh.ListTeamMembers(ctx, org, diag.teamSlug)
	if err != nil {
		fmt.Printf("      members: error: %v\n", err)
		return
	}
	if len(members) == 0 {
		fmt.Printf("      members: none\n")
		return
	}

	var logins []string
	for _, member := range members {
		logins = append(logins, member.Login)
	}
	sort.Strings(logins)
	fmt.Printf("      members (%d): %s\n", len(logins), strings.Join(logins, ", "))
}

func splitAndClean(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func exitErr(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "diagnose: %v\n", err)
	os.Exit(1)
}
