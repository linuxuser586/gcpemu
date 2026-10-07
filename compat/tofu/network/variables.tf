# Set by TestOpenTofu (compat/tofu_test.go), which also adds the provider
# requirements and the `gcpemu tofu-provider` block.

variable "project" {
  type = string
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "zone" {
  type    = string
  default = "us-central1-a"
}
