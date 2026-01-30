package naming

import (
	"regexp"
	"strings"
)

var slugExpression = regexp.MustCompile(`[^a-z0-9]+`)

// SlugifyTeamName converts a team display name into a GitHub slug (lowercase, hyphen separated).
func SlugifyTeamName(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return ""
	}
	slug := slugExpression.ReplaceAllString(lower, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "team"
	}
	return slug
}
