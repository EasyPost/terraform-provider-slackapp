package provider_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func installationConfig(f *fakeSlack, scope string) string {
	return fmt.Sprintf(`
provider "slackapp" {
  base_url = %q
  developer_token = "developer-test"
}
resource "slackapp_installation" "test" {
  app_id = "A1"
  workspace_id = "T1"
  bot_scopes = [%q]
}
`, f.baseURL(), scope)
}

func TestAccInstallationLifecycle(t *testing.T) {
	providerUnsetEnvironment(t)
	f := newFakeSlack(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactories,
		CheckDestroy: func(*terraform.State) error {
			if calls := f.callCount("apps.developerUninstall"); calls != 1 {
				return fmt.Errorf("uninstall calls = %d, want 1", calls)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: installationConfig(f, "chat:write"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("slackapp_installation.test", "id", "A1/T1"),
					resource.TestCheckResourceAttr("slackapp_installation.test", "installed", "true"),
					resource.TestCheckResourceAttr("slackapp_installation.test", "bot_token", "bot-A1-1"),
				),
			},
			{
				Config: installationConfig(f, "users:read"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("slackapp_installation.test", "bot_token", "bot-A1-2"),
					resource.TestCheckResourceAttr("slackapp_installation.test", "bot_scopes.0", "users:read"),
				),
			},
			{
				ResourceName: "slackapp_installation.test", ImportState: true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"bot_scopes", "outgoing_domains", "bot_token", "app_level_token", "user_token"},
			},
		},
	})
}

func TestAccInstallationWrongWorkspaceDoesNotMutate(t *testing.T) {
	providerUnsetEnvironment(t)
	f := newFakeSlack(t)
	f.failWith("auth.test", map[string]any{"ok": true, "team_id": "T2", "user_id": "U1"})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactories,
		Steps: []resource.TestStep{{
			Config:      installationConfig(f, "chat:write"),
			ExpectError: regexp.MustCompile("exact target workspace"),
		}},
	})
	if f.callCount("apps.developerInstall") != 0 {
		t.Fatal("wrong-workspace token reached install")
	}
}

func TestAccInstallationApprovalIsNotInstallation(t *testing.T) {
	providerUnsetEnvironment(t)
	f := newFakeSlack(t)
	f.failWith("apps.developerInstall", map[string]any{"ok": false, "error": "app_approval_request_pending"})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactories,
		Steps: []resource.TestStep{{
			Config:      installationConfig(f, "chat:write"),
			ExpectError: regexp.MustCompile("app_approval_request_pending"),
		}},
	})
	if f.callCount("apps.developerInstall") != 1 {
		t.Fatal("approval rejection must not be retried")
	}
}

func TestAccInstallationInventoryPreservesOmittedApps(t *testing.T) {
	providerUnsetEnvironment(t)
	f := newFakeSlack(t)
	f.failWith("apps.status", map[string]any{
		"ok": true, "team": map[string]string{"team_id": "T1"},
		"apps": []map[string]any{
			{"app_id": "A1", "is_installed": true},
			{"app_id": "A2", "is_installed": false},
		},
	})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "slackapp" {
  base_url = %q
  developer_token = "developer-test"
}
data "slackapp_installation_inventory" "test" {
  workspace_id = "T1"
  app_ids = ["A1", "A2", "A3"]
}
`, f.baseURL()),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.slackapp_installation_inventory.test", "statuses.A1", "installed"),
				resource.TestCheckResourceAttr("data.slackapp_installation_inventory.test", "statuses.A2", "uninstalled"),
				resource.TestCheckResourceAttr("data.slackapp_installation_inventory.test", "statuses.A3", "unknown"),
				resource.TestCheckResourceAttr("data.slackapp_installation_inventory.test", "all_requested_apps_observed", "false"),
			),
		}},
	})
	if f.callCount("apps.developerInstall") != 0 || f.callCount("apps.developerUninstall") != 0 {
		t.Fatal("inventory performed a mutation")
	}
}

func TestAccInstallationUncertainCreateReconcilesWithoutReplacement(t *testing.T) {
	for name, test := range map[string]struct {
		reply           map[string]any
		mutationFailure bool
	}{
		"omitted": {
			reply: map[string]any{"ok": true, "team": map[string]string{"team_id": "T1"}, "apps": []map[string]any{}},
		},
		"rejected confirmation": {reply: map[string]any{"ok": false, "error": "invalid_auth"}},
		"partial execution": {
			reply: map[string]any{"ok": false, "error": "fatal_error"}, mutationFailure: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			providerUnsetEnvironment(t)
			f := newFakeSlack(t)
			tokenCheck := resource.TestCheckResourceAttr("slackapp_installation.test", "bot_token", "bot-A1-1")
			if test.mutationFailure {
				f.failInstallationAfterMutation(test.reply)
				tokenCheck = resource.TestCheckNoResourceAttr("slackapp_installation.test", "bot_token")
			} else {
				f.failInstallationConfirmation(true, test.reply)
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: providerFactories,
				Steps: []resource.TestStep{
					{
						Config:      installationConfig(f, "chat:write"),
						ExpectError: regexp.MustCompile("Installation requires reconciliation"),
					},
					{
						RefreshState: true,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("slackapp_installation.test", "id", "A1/T1"),
							tokenCheck,
						),
					},
					{
						Config: installationConfig(f, "chat:write"),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("slackapp_installation.test", "installed", "true"),
							func(*terraform.State) error {
								if f.callCount("apps.developerInstall") != 1 || f.callCount("apps.developerUninstall") != 0 {
									return fmt.Errorf("reconciliation replaced the installation")
								}
								return nil
							},
						),
					},
				},
			})
		})
	}
}

func TestAccInstallationUnknownReadsPreserveState(t *testing.T) {
	providerUnsetEnvironment(t)
	f := newFakeSlack(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{Config: installationConfig(f, "chat:write")},
			{
				PreConfig: func() {
					f.failWith("apps.status", map[string]any{
						"ok": true, "team": map[string]string{"team_id": "T1"}, "apps": []map[string]any{},
					})
				},
				Config:      installationConfig(f, "chat:write"),
				ExpectError: regexp.MustCompile("Installation status is unknown"),
			},
			{
				PreConfig:   func() { f.failWith("apps.status", map[string]any{"ok": false, "error": "invalid_auth"}) },
				Config:      installationConfig(f, "chat:write"),
				ExpectError: regexp.MustCompile("Unable to observe installation"),
			},
			{
				PreConfig: func() { f.succeed("apps.status") },
				Config:    installationConfig(f, "chat:write"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("slackapp_installation.test", "id", "A1/T1"),
					resource.TestCheckResourceAttr("slackapp_installation.test", "bot_token", "bot-A1-1"),
					func(*terraform.State) error {
						if f.callCount("apps.developerInstall") != 1 || f.callCount("apps.developerUninstall") != 0 {
							return fmt.Errorf("unknown reads caused a mutation")
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAccInstallationFailedReinstallKeepsPriorConfiguration(t *testing.T) {
	providerUnsetEnvironment(t)
	f := newFakeSlack(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{Config: installationConfig(f, "chat:write")},
			{
				PreConfig: func() {
					f.failInstallationConfirmation(true, map[string]any{
						"ok": true, "team": map[string]string{"team_id": "T1"}, "apps": []map[string]any{},
					})
				},
				Config:      installationConfig(f, "users:read"),
				ExpectError: regexp.MustCompile("Reinstall attempt was not confirmed"),
			},
			{
				Config: installationConfig(f, "chat:write"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("slackapp_installation.test", "bot_scopes.0", "chat:write"),
					resource.TestCheckResourceAttr("slackapp_installation.test", "bot_token", "bot-A1-2"),
					func(*terraform.State) error {
						if f.callCount("apps.developerInstall") != 2 {
							return fmt.Errorf("unexpected additional reinstall")
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAccInstallationUnconfirmedDeleteRetainsIdentity(t *testing.T) {
	providerUnsetEnvironment(t)
	f := newFakeSlack(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{Config: installationConfig(f, "chat:write")},
			{
				PreConfig: func() { f.failWith("apps.developerUninstall", map[string]any{"ok": false, "error": "not_allowed"}) },
				Config:    installationConfig(f, "chat:write"), Destroy: true,
				ExpectError: regexp.MustCompile("Uninstall attempt was not confirmed"),
			},
			{
				PreConfig: func() { f.succeed("apps.developerUninstall") },
				Config:    installationConfig(f, "chat:write"),
				Check:     resource.TestCheckResourceAttr("slackapp_installation.test", "id", "A1/T1"),
			},
			{
				PreConfig: func() {
					f.failInstallationConfirmation(false, map[string]any{
						"ok": true, "team": map[string]string{"team_id": "T1"},
						"apps": []map[string]any{{"app_id": "A1", "is_installed": true}},
					})
				},
				Config: installationConfig(f, "chat:write"), Destroy: true,
				ExpectError: regexp.MustCompile("Uninstall attempt was not confirmed"),
			},
			{
				Config: fmt.Sprintf(`
provider "slackapp" {
  base_url = %q
  developer_token = "developer-test"
}
`, f.baseURL()),
				Check: func(state *terraform.State) error {
					if _, exists := state.RootModule().Resources["slackapp_installation.test"]; exists {
						return fmt.Errorf("confirmed absent installation was not removed")
					}
					if f.callCount("apps.developerInstall") != 1 || f.callCount("apps.developerUninstall") != 2 {
						return fmt.Errorf("delete reconciliation made an additional mutation")
					}
					return nil
				},
			},
		},
	})
}
