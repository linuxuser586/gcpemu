# SRS 11.2 reference stack: one OpenTofu root module creating everything
# step 1 lists. It targets real GCP unless emulator_gateway is set, in
# which case every API the stack uses is pointed at that gcpemu gateway
# (the same settings `gcpemu tofu-provider` prints).

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 7.0, < 9.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = ">= 4.0, < 5.0"
    }
  }
}

locals {
  emu = var.emulator_gateway != "" ? trimsuffix(var.emulator_gateway, "/") : ""
  # endpoint returns the emulator base path for an API, or null (the
  # provider default) when running against GCP.
  endpoints = {
    iam                 = "/iam/v1/"
    iam_credentials     = "/iamcredentials/v1/"
    resource_manager    = "/cloudresourcemanager/v1/"
    resource_manager_v3 = "/cloudresourcemanager/v3/"
    storage             = "/storage/v1/"
    pubsub              = "/pubsub/v1/"
    dns                 = "/dns/v1/"
    artifact_registry   = "/artifactregistry/v1/"
    compute             = "/compute/v1/"
    service_networking  = "/servicenetworking/v1/"
    container           = "/container/v1/"
    sql                 = "/sql/v1beta4/"
    certificate_manager = "/certificatemanager/v1/"
    network_security    = "/networksecurity/v1/"
  }
  ep = { for k, v in local.endpoints : k => local.emu != "" ? "${local.emu}${v}" : null }
}

provider "google" {
  project = var.project
  region  = var.region
  zone    = var.zone

  # The emulator accepts "owner" as the default principal's token.
  access_token = local.emu != "" ? "owner" : null

  iam_custom_endpoint                 = local.ep.iam
  iam_beta_custom_endpoint            = local.ep.iam
  iam_credentials_custom_endpoint     = local.ep.iam_credentials
  resource_manager_custom_endpoint    = local.ep.resource_manager
  resource_manager_v3_custom_endpoint = local.ep.resource_manager_v3
  storage_custom_endpoint             = local.ep.storage
  pubsub_custom_endpoint              = local.ep.pubsub
  dns_custom_endpoint                 = local.ep.dns
  artifact_registry_custom_endpoint   = local.ep.artifact_registry
  compute_custom_endpoint             = local.ep.compute
  service_networking_custom_endpoint  = local.ep.service_networking
  container_custom_endpoint           = local.ep.container
  sql_custom_endpoint                 = local.ep.sql
  certificate_manager_custom_endpoint = local.ep.certificate_manager
  network_security_custom_endpoint    = local.ep.network_security

  add_terraform_attribution_label = false
}
