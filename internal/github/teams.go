package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/CaseyLabs/okta-github-team-sync/internal/naming"
	"github.com/CaseyLabs/okta-github-team-sync/internal/util"
)

// Mapping describes a GitHub Team Sync IdP group association.
type Mapping struct {
	GroupID          string    `json:"group_id"`
	GroupName        string    `json:"group_name"`
	GroupDescription string    `json:"group_description"`
	Status           string    `json:"status,omitempty"`
	SyncedAt         time.Time `json:"synced_at,omitempty"`
}

type teamResponse struct {
	Slug string `json:"slug"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// TeamDetails represents a GitHub team record.
type TeamDetails struct {
	Slug string
	ID   int64
	Name string
}

// TeamMember captures a GitHub username associated with a team.
type TeamMember struct {
	Login string
}

// EnsureTeam fetches an existing team or creates it if missing, returning the slug.
func (c *Client) EnsureTeam(ctx context.Context, org, name string) (string, error) {
	slug := naming.SlugifyTeamName(name)

	team, err := c.getTeam(ctx, org, slug)
	if err == nil {
		return team.Slug, nil
	}

	if !isNotFound(err) {
		return "", fmt.Errorf("get team %q in org %q: %w", name, org, err)
	}

	body := map[string]any{
		"name":    name,
		"privacy": "closed",
	}

	var created teamResponse
	if err := c.doJSON(ctx, nethttp.MethodPost, fmt.Sprintf("/orgs/%s/teams", org), org, body, &created); err != nil {
		return "", fmt.Errorf("create team %q in org %q: %w", name, org, err)
	}
	return created.Slug, nil
}

// GetTeamDetails retrieves the GitHub team information for the provided slug.
func (c *Client) GetTeamDetails(ctx context.Context, org, slug string) (*TeamDetails, error) {
	team, err := c.getTeam(ctx, org, slug)
	if err != nil {
		return nil, err
	}
	return &TeamDetails{Slug: team.Slug, ID: team.ID, Name: team.Name}, nil
}

// TeamExists returns true when the team already exists in the organization.
func (c *Client) TeamExists(ctx context.Context, org, slug string) (bool, error) {
	_, err := c.getTeam(ctx, org, slug)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// GetGroupMappings returns the Team Sync group mappings for the team.
func (c *Client) GetGroupMappings(ctx context.Context, org, slug string) ([]Mapping, error) {
	path := fmt.Sprintf("/orgs/%s/teams/%s/team-sync/group-mappings", org, slug)
	var payload struct {
		Groups []Mapping `json:"groups"`
	}

	if err := c.doJSON(ctx, nethttp.MethodGet, path, org, nil, &payload); err != nil {
		return nil, fmt.Errorf("get group mappings for %s/%s: %w", org, slug, err)
	}

	return payload.Groups, nil
}

// PatchGroupMappings replaces the Team Sync group mappings for the team.
func (c *Client) PatchGroupMappings(ctx context.Context, org, slug string, groups []Mapping) error {
	path := fmt.Sprintf("/orgs/%s/teams/%s/team-sync/group-mappings", org, slug)
	body := map[string]any{"groups": groups}

	if err := c.doJSON(ctx, nethttp.MethodPatch, path, org, body, nil); err != nil {
		return fmt.Errorf("patch group mappings for %s/%s: %w", org, slug, err)
	}

	return nil
}

// SetTeamPermission updates the team's organization-level permission (maintain, admin, push, pull).
func (c *Client) SetTeamPermission(ctx context.Context, org, slug, permission string) error {
	path := fmt.Sprintf("/orgs/%s/teams/%s", org, slug)
	body := map[string]any{"permission": permission}

	if err := c.doJSON(ctx, nethttp.MethodPatch, path, org, body, nil); err != nil {
		return fmt.Errorf("set team permission for %s/%s: %w", org, slug, err)
	}

	return nil
}

// ListTeamMembers returns the logins associated with the GitHub team.
func (c *Client) ListTeamMembers(ctx context.Context, org, slug string) ([]TeamMember, error) {
	path := fmt.Sprintf("/orgs/%s/teams/%s/members?per_page=100", org, slug)
	var members []TeamMember

	for path != "" {
		req, err := c.newRequest(ctx, nethttp.MethodGet, path, org, nil)
		if err != nil {
			return nil, err
		}

		resp, err := util.DoRaw(ctx, c.httpClient, req)
		if err != nil {
			return nil, err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response for %s/%s members: %w", org, slug, err)
		}

		if util.HTTPDebugEnabled() {
			util.LogHTTPResponseBody(req.Method, req.URL.String(), resp.StatusCode, body)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			bodyStr := strings.TrimSpace(string(body))
			if len(bodyStr) > 4096 {
				bodyStr = bodyStr[:4096]
			}
			return nil, fmt.Errorf("list team members for %s/%s: %w", org, slug, &util.HTTPError{StatusCode: resp.StatusCode, Body: bodyStr, URL: req.URL.String()})
		}

		var page []struct {
			Login string `json:"login"`
		}

		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode team members for %s/%s: %w", org, slug, err)
		}

		for _, m := range page {
			if m.Login == "" {
				continue
			}
			members = append(members, TeamMember{Login: m.Login})
		}

		next := parseNextLink(resp.Header.Get("Link"))
		if next == "" {
			break
		}
		if strings.HasPrefix(next, c.BaseURL) {
			next = strings.TrimPrefix(next, c.BaseURL)
		}
		path = next
	}

	return members, nil
}

func (c *Client) getTeam(ctx context.Context, org, slug string) (*teamResponse, error) {
	path := fmt.Sprintf("/orgs/%s/teams/%s", org, slug)
	var resp teamResponse
	if err := c.doJSON(ctx, nethttp.MethodGet, path, org, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func isNotFound(err error) bool {
	var httpErr *util.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == nethttp.StatusNotFound
}

func parseNextLink(header string) string {
	if header == "" {
		return ""
	}

	parts := strings.Split(header, ",")
	for _, part := range parts {
		segments := strings.Split(strings.TrimSpace(part), ";")
		if len(segments) == 0 {
			continue
		}

		link := strings.Trim(strings.TrimSpace(segments[0]), "<>")
		rel := ""
		for _, seg := range segments[1:] {
			seg = strings.TrimSpace(seg)
			if strings.HasPrefix(seg, "rel=") {
				rel = strings.Trim(seg[len("rel="):], "\"")
			}
		}

		if rel == "next" {
			return link
		}
	}

	return ""
}
