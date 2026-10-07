variable "emulator_gateway" {
  description = "gcpemu gateway URL (e.g. http://127.0.0.1:4510); empty = real GCP"
  type        = string
  default     = ""
}

variable "project" {
  type    = string
  default = "refstack"
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "zone" {
  type    = string
  default = "us-central1-a"
}

variable "domain" {
  description = "DNS zone (with trailing dot) and the app hostname's parent"
  type        = string
  default     = "example.test."
}

variable "node_count" {
  type    = number
  default = 1
}

variable "api_neg_name" {
  description = <<-EOT
    Name of the GKE standalone NEG of the Istio ingress gateway Service
    (cloud.google.com/neg annotation). The NEG only exists once the
    gateway is installed, so the first apply leaves it empty and the API
    backend service and its /api/* route are added by a second apply.
  EOT
  type        = string
  default     = ""
}

variable "nat_enabled" {
  description = "Cloud NAT for the private nodes (step 7 turns it off)"
  type        = bool
  default     = true
}

variable "app_storage_access" {
  description = "Grant the app's GSA roles/storage.objectAdmin on the uploads bucket (step 8 revokes it)"
  type        = bool
  default     = true
}

variable "db_admin_password" {
  description = "Password of the built-in admin user used for the schema migration"
  type        = string
  sensitive   = true
  default     = "change-me-0123456789"
}
