terraform {
  required_providers {
    slackapp = {
      source = "registry.terraform.io/easypost/slackapp"
    }
  }
}

provider "slackapp" {}

variable "workspace_id" {
  description = "Workspace to which the Slack CLI service token belongs."
  type        = string
  nullable    = false
}

variable "third_party_app_ids" {
  description = "Known app IDs from a reviewed catalog; visibility is token-dependent."
  type        = set(string)
  default     = []
  nullable    = false
}

resource "slackapp_application" "owned" {
  manifest = jsonencode(yamldecode(file("${path.module}/app.yaml")))

  lifecycle {
    prevent_destroy = true
  }
}

resource "slackapp_installation" "owned" {
  app_id       = slackapp_application.owned.id
  workspace_id = var.workspace_id
  bot_scopes   = toset(jsondecode(slackapp_application.owned.manifest).oauth_config.scopes.bot)

  lifecycle {
    prevent_destroy = true
  }
}

data "slackapp_installation_inventory" "workspace" {
  workspace_id = var.workspace_id
  app_ids      = setunion([slackapp_application.owned.id], var.third_party_app_ids)
  depends_on   = [slackapp_installation.owned]
}

output "installation_observations" {
  description = "Observed status for the requested IDs; does not establish full workspace coverage."
  value       = data.slackapp_installation_inventory.workspace.statuses
}
