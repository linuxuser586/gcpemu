variable "emulator_gateway" {
  description = "gcpemu gateway URL (e.g. http://127.0.0.1:4562); empty = real GCP"
  type        = string
  default     = ""
}

variable "project" {
  type    = string
  default = "autopilot"
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "kubernetes_version" {
  description = "The cluster's minimum version, from the Rapid channel's valid versions; empty = the channel default"
  type        = string
  default     = ""
}
