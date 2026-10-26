# SRS 11.4 Autopilot acceptance: an Autopilot cluster from
# enable_autopilot = true. It targets real GCP unless emulator_gateway is
# set, in which case the APIs it uses are pointed at that gcpemu gateway.

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 7.2, < 9.0"
    }
  }
}

locals {
  emu = var.emulator_gateway != "" ? trimsuffix(var.emulator_gateway, "/") : ""
}

provider "google" {
  project = var.project
  region  = var.region

  # The emulator accepts "owner" as the default principal's token.
  access_token = local.emu != "" ? "owner" : null
  # Skips the provider's userinfo lookup (#155; needs provider >= 7.2).
  universe_domain = local.emu != "" ? "gcpemu.invalid" : null

  compute_custom_endpoint   = local.emu != "" ? "${local.emu}/compute/v1/" : null
  container_custom_endpoint = local.emu != "" ? "${local.emu}/container/v1/" : null

  add_terraform_attribution_label = false
}
