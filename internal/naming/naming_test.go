package naming

import "testing"

func TestSlugifyTeamName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Platform Engineering", "platform-engineering"},
		{"Data & Analytics", "data-analytics"},
		{"  SRE__Core  ", "sre-core"},
		{"!@#", "team"},
	}

	for _, tt := range tests {
		if got := SlugifyTeamName(tt.name); got != tt.want {
			t.Errorf("SlugifyTeamName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
