# Backend mTLS between the load balancer and the Istio ingress gateway
# (FR-LB-006, FR-INT-002):
#
#   - the gateway serves a certificate from the "mesh" CA, which the LB
#     trusts through a Certificate Manager trust config;
#   - the LB presents a CLIENT_AUTH certificate from the "lb-client" CA,
#     which the gateway (Gateway tls.mode MUTUAL) requires.
#
# The gateway's certificate, key and the client CA are outputs the Istio
# installation step turns into the gateway's credential Secret.

locals {
  gateway_name = "gateway.mesh.internal"
}

resource "tls_private_key" "mesh_ca" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "mesh_ca" {
  private_key_pem       = tls_private_key.mesh_ca.private_key_pem
  is_ca_certificate     = true
  validity_period_hours = 24 * 365
  allowed_uses          = ["cert_signing", "crl_signing", "digital_signature"]
  subject {
    common_name  = "Reference stack mesh CA"
    organization = "gcpemu"
  }
}

resource "tls_private_key" "gateway" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_cert_request" "gateway" {
  private_key_pem = tls_private_key.gateway.private_key_pem
  dns_names       = [local.gateway_name]
  subject {
    common_name = local.gateway_name
  }
}

resource "tls_locally_signed_cert" "gateway" {
  cert_request_pem      = tls_cert_request.gateway.cert_request_pem
  ca_private_key_pem    = tls_private_key.mesh_ca.private_key_pem
  ca_cert_pem           = tls_self_signed_cert.mesh_ca.cert_pem
  validity_period_hours = 24 * 90
  allowed_uses          = ["digital_signature", "key_encipherment", "server_auth"]
}

resource "tls_private_key" "client_ca" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "client_ca" {
  private_key_pem       = tls_private_key.client_ca.private_key_pem
  is_ca_certificate     = true
  validity_period_hours = 24 * 365
  allowed_uses          = ["cert_signing", "crl_signing", "digital_signature"]
  subject {
    common_name  = "Reference stack LB client CA"
    organization = "gcpemu"
  }
}

resource "tls_private_key" "lb_client" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_cert_request" "lb_client" {
  private_key_pem = tls_private_key.lb_client.private_key_pem
  dns_names       = ["lb.${trimsuffix(var.domain, ".")}"]
  subject {
    common_name = "lb.${trimsuffix(var.domain, ".")}"
  }
}

resource "tls_locally_signed_cert" "lb_client" {
  cert_request_pem      = tls_cert_request.lb_client.cert_request_pem
  ca_private_key_pem    = tls_private_key.client_ca.private_key_pem
  ca_cert_pem           = tls_self_signed_cert.client_ca.cert_pem
  validity_period_hours = 24 * 90
  allowed_uses          = ["digital_signature", "key_encipherment", "client_auth"]
}

resource "google_certificate_manager_certificate" "lb_client" {
  name  = "ref-lb-client"
  scope = "CLIENT_AUTH"
  self_managed {
    pem_certificate = tls_locally_signed_cert.lb_client.cert_pem
    pem_private_key = tls_private_key.lb_client.private_key_pem
  }
}

resource "google_certificate_manager_trust_config" "mesh" {
  name     = "ref-mesh"
  location = "global"
  trust_stores {
    trust_anchors {
      pem_certificate = tls_self_signed_cert.mesh_ca.cert_pem
    }
  }
}

resource "google_network_security_backend_authentication_config" "lb" {
  name               = "ref-lb-mtls"
  location           = "global"
  client_certificate = google_certificate_manager_certificate.lb_client.id
  trust_config       = google_certificate_manager_trust_config.mesh.id
  well_known_roots   = "NONE"
}
