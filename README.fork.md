# EasyPost Slack app provider

This development fork adds `slackapp_installation` for owned bot apps and `slackapp_installation_inventory` for read-only observations of supplied app IDs. Existing manifest management remains available. These features are experimental: tests exercise OpenTofu against a local fake API, and no real installation, token, third-party visibility, or production deployment has been verified.

The API boundary is defined in [contracts/slack-source.yaml](contracts/slack-source.yaml). This provider-owned OpenAPI contract is inferred from Slack's official CLI [installation and status protocol](https://github.com/slackapi/slack-cli/blob/6e69bce86ca32b1a90604000e43e033759e3ea4d/internal/api/app.go), [session validation](https://github.com/slackapi/slack-cli/blob/6e69bce86ca32b1a90604000e43e033759e3ea4d/internal/api/session.go) and the public [`auth.test` reference](https://docs.slack.dev/reference/methods/auth.test/). tfspecgen generates the API client and operation/model introspection; the handwritten adapter owns workspace validation, Slack error envelopes and installation reconciliation. See [contract regeneration and limits](contracts/README.md). The installation methods follow Slack CLI's backend protocol; validate account access and compatibility before adopting them.

## Authentication

Set `SLACK_DEVELOPER_TOKEN` to a workspace-scoped Slack CLI service token. Slack documents [service-token authorization for CI/CD](https://docs.slack.dev/tools/slack-cli/guides/authorizing-the-slack-cli/#cicd-authorization); its initial authorization still requires a user. This token is separate from `SLACK_APP_CONFIGURATION_TOKEN` and `SLACK_REFRESH_TOKEN`, which remain necessary for manifest management. The provider does not bootstrap or rotate service tokens.

Every installation/status call first checks `auth.test` for a user and the exact target workspace. Organization-scoped service tokens and organization-wide grants are unsupported. A different workspace, missing user, or API failure stops the operation. Administrator approval errors remain errors and do not count as installation; the provider does not submit or bypass approval requests.

Installation can return bot, user, and app-level tokens. These are sensitive resource attributes, retained in state; protect the backend and saved plans. They are not printed in API logs or recovered by reading/importing an installation. Preserve existing runtime credential handoffs before adoption.

## Lifecycle

Create first requires an explicit uninstalled observation, then calls `apps.developerInstall` and checks `apps.status` for confirmation. An existing installation must be imported. Changes to requested bot scopes or outgoing domains reinstall the app. The requested scopes are configuration inputs and do not attest the permissions of every existing token.

A definitive authentication, scope, or approval refusal fails create without recording an installation. Backend and unknown errors can follow partial execution and require reconciliation. An unconfirmed create retains its app/workspace identity and validated returned tokens with a warning and `installed = null`. Returning a create error with saved state would taint the resource and schedule replacement, so this path uses a warning and blocks subsequent planning until refresh reconciles the result. Refresh confirms an installation or removes a confirmed absent object. Missing app IDs or installation flags remain unknown; read errors preserve state. Delete calls `apps.developerUninstall` only after observing the installation, and retains state until absence is confirmed; it does not delete the app definition. Mutations are not automatically retried.

Import uses `APP_ID/WORKSPACE_ID`. Status reads cannot reconstruct prior requested scopes or credentials. Review the first plan after import: supplying bot scopes can schedule a reinstall. Use `lifecycle.prevent_destroy` on both the app definition and installation when deletion needs review.

## Inventory

Supply app IDs from a reviewed catalog or export. The data source returns an entry for every requested ID: `installed`, `uninstalled`, or `unknown`, plus the observation date and whether every requested ID was observed. Owned and third-party IDs can be included; a service token's ability to observe third-party IDs must be verified. An omitted ID is never treated as uninstalled.

This method checks supplied IDs and cannot discover every app installed in a workspace. To populate the catalog, Enterprise administrators can use Slack's [Installed Apps CSV export](https://slack.com/help/articles/360000281563-Manage-apps-in-an-Enterprise-organization), subject to plan and admin access. Verify the export fields and coverage before building an importer. Approval lists, requested IDs, and a successful status request do not establish complete workspace coverage. Keep owners and purpose in repository catalog data and raw evidence in access-controlled storage.

## Development example

[examples/installations](examples/installations/main.tf) composes a manifest, an installation, and an inventory containing the owned app plus caller-supplied third-party IDs. It uses synthetic app configuration and requires a workspace ID and external credentials. Do not apply it to a real workspace during fixture checks.

The fork serves `registry.terraform.io/easypost/slackapp`, but that address has no published release yet. For local schema validation, build the binary into an isolated directory and use a temporary OpenTofu CLI configuration with a development override:

```hcl
provider_installation {
  dev_overrides {
    "registry.terraform.io/easypost/slackapp" = "/absolute/path/to/provider-build"
  }
  direct {}
}
```

```shell
go build -o /absolute/path/to/provider-build/terraform-provider-slackapp .
TF_CLI_CONFIG_FILE=/absolute/path/to/development.tfrc tofu -chdir=examples/installations validate
TF_ACC_PROVIDER_HOST=registry.opentofu.org TF_ACC_TERRAFORM_PATH="$(command -v tofu)" gotestsum -- -race ./...
```

Skip `init` with the development override. The override runs a local binary and is not a registry release or deployment. Real app adoption, protected state, durable configuration-token storage, executor credentials, and a controlled installation probe remain separate gates.
