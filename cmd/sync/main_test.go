package main

import "testing"

func TestParseInstallationOrgListUsesNewlines(t *testing.T) {
	raw := "org-a:1\norg-b:2\r\norg-c:3,org-d:4"
	orgs, err := parseInstallationOrgList(raw)
	if err != nil {
		t.Fatalf("parseInstallationOrgList error: %v", err)
	}
	expected := []string{"org-a", "org-b", "org-c", "org-d"}
	if len(orgs) != len(expected) {
		t.Fatalf("unexpected org count: got %d want %d", len(orgs), len(expected))
	}
	for i, org := range expected {
		if orgs[i] != org {
			t.Fatalf("unexpected org at %d: got %s want %s", i, orgs[i], org)
		}
	}
}

func TestLoadConfigFallsBackToInstallationIDs(t *testing.T) {
	t.Setenv(groupPrefixEnvVar, "Okta-Team-")
	t.Setenv("OKTA_TEAM_SYNC_GH_INSTALLATION_IDS", "org-a:1,org-b:2")
	t.Setenv("DRY_RUN", "true")
	t.Setenv("STATE_PATH", "state.json")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	expected := []string{"org-a", "org-b"}
	if len(cfg.Organizations) != len(expected) {
		t.Fatalf("unexpected org count: got %d want %d", len(cfg.Organizations), len(expected))
	}
	for i, org := range expected {
		if cfg.Organizations[i] != org {
			t.Fatalf("unexpected org at %d: got %s want %s", i, cfg.Organizations[i], org)
		}
	}
	if !cfg.DryRun {
		t.Fatalf("expected dry run true")
	}
	if cfg.StatePath != "state.json" {
		t.Fatalf("unexpected state path: %s", cfg.StatePath)
	}
}

func TestLoadConfigPrefersExplicitOrgList(t *testing.T) {
	t.Setenv(groupPrefixEnvVar, "Okta-Team-")
	t.Setenv("ORG_LIST", "One,Two")
	t.Setenv("OKTA_TEAM_SYNC_GH_INSTALLATION_IDS", "org-a:1")
	t.Setenv("DRY_RUN", "false")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	expected := []string{"one", "two"}
	if len(cfg.Organizations) != len(expected) {
		t.Fatalf("unexpected org count: got %d want %d", len(cfg.Organizations), len(expected))
	}
	for i, org := range expected {
		if cfg.Organizations[i] != org {
			t.Fatalf("unexpected org at %d: got %s want %s", i, cfg.Organizations[i], org)
		}
	}
	if cfg.DryRun {
		t.Fatalf("expected dry run false")
	}
}

func TestLoadConfigFailsWithoutSources(t *testing.T) {
	t.Setenv(groupPrefixEnvVar, "Okta-Team-")
	t.Setenv("ORG_LIST", "")
	t.Setenv("OKTA_TEAM_SYNC_GH_INSTALLATION_IDS", "")

	if _, err := loadConfig(); err == nil {
		t.Fatalf("expected error when no org sources provided")
	}
}

func TestParseOktaAppIDsStripsComments(t *testing.T) {
	t.Setenv("OKTA_GITHUB_APP_IDS", "id-one # primary\nid-two#secondary , id-three   #  third ")
	ids := parseOktaAppIDs()
	expected := []string{"id-one", "id-two", "id-three"}
	if len(ids) != len(expected) {
		t.Fatalf("unexpected count: got %d want %d", len(ids), len(expected))
	}
	for i, id := range expected {
		if ids[i] != id {
			t.Fatalf("unexpected id at %d: got %s want %s", i, ids[i], id)
		}
	}
}
