# Slack API contract

[slack-source.yaml](slack-source.yaml) defines the four actions used by installation and inventory: `auth.test`, `apps.status`, `apps.developerInstall` and `apps.developerUninstall`. This is a provider-owned interpretation of Slack CLI commit [`6e69bce86ca32b1a90604000e43e033759e3ea4d`](https://github.com/slackapi/slack-cli/tree/6e69bce86ca32b1a90604000e43e033759e3ea4d), not an official Slack OpenAPI specification.

Installation and status request bodies and response fields follow [`internal/api/app.go`](https://github.com/slackapi/slack-cli/blob/6e69bce86ca32b1a90604000e43e033759e3ea4d/internal/api/app.go) (`DeveloperAppInstall`, `UninstallApp`, `GetAppStatus`). Authentication fields follow [`internal/api/session.go`](https://github.com/slackapi/slack-cli/blob/6e69bce86ca32b1a90604000e43e033759e3ea4d/internal/api/session.go) (`ValidateSession`) and Slack's [`auth.test` reference](https://docs.slack.dev/reference/methods/auth.test/), including its bot-token response and JSON/Bearer request support. The contract covers the workspace user service-token path; organization grants, approval submission and discovery of all installed apps remain outside its scope.

Requiring `ok`, accepting nullable/omitted success fields and retaining unknown observations are provider safeguards. HTTP 200 does not imply Slack accepted an action. The generated introspection's `is_success` describes HTTP status, so the adapter must also check `ok`. An omitted/null installation flag remains unknown, and a missing/null app list invalidates the observation. The contract and local fake API do not establish live Slack compatibility.

## Regeneration

The pipeline follows our Looker provider's tfspecgen approach: checked-in source, path safelist, ogen client, then operation/model introspection. [tfspecgen.yaml](../tfspecgen.yaml) describes the source, and [mise.toml](../mise.toml) pins the public generators. Install the private tfspecgen module with authorized repository access at the version in [.tfspecgen-version](../.tfspecgen-version), then run:

```shell
mise install
mise run codegen
mise run codegen-check
```

The outputs are [internal/slackapi](../internal/slackapi) and [generated/slack_ogen_introspection.json](../generated/slack_ogen_introspection.json). Do not edit generated code or introspection. The pinned introspector requires `oas_parameters_gen.go` even when ogen emits no path/query parameters; regeneration supplies an empty AST only in `.codegen/slackapi`, records that staging path in introspection, and removes staging on exit.

Public CI uses pinned ogen to regenerate and compare the client. It also checks a SHA-256 snapshot tying the last complete regeneration's contract, configuration, client and introspection together. It does not run the private tfspecgen introspector; a full regeneration requires authorized local access. No private tool source or credentials are vendored into this public fork.

## Handwritten boundaries

[internal/slack/installation.go](../internal/slack/installation.go) uses generated request/response types and security support. It adds credential isolation, workspace/app identity checks, bounded responses, redacted failures and no mutation redirects. Existing manifest APIs keep their original client.

The installation resource and inventory data source remain handwritten. Installation has an uncertain create state, reinstall semantics, credential retention and confirmed deletion; inventory submits IDs in a POST body and translates a partial response into three observation states. These need behavior beyond tfspecgen's current ordinary CRUD and path/query data-source bindings. The empty `resources` and `data_sources` maps explicitly declare that boundary.
