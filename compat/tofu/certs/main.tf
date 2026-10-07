# Certificate Manager and Network Security: self-managed and
# DNS-authorized certificates, certificate maps, trust configs and the TLS
# policies and backend authentication config they feed.

resource "tls_private_key" "ca" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "ca" {
  private_key_pem       = tls_private_key.ca.private_key_pem
  is_ca_certificate     = true
  validity_period_hours = 24 * 365
  allowed_uses          = ["cert_signing", "crl_signing", "digital_signature"]
  subject {
    common_name = "acceptance CA"
  }
}

resource "tls_private_key" "leaf" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_cert_request" "leaf" {
  private_key_pem = tls_private_key.leaf.private_key_pem
  dns_names       = ["certs.example.test"]
  subject {
    common_name = "certs.example.test"
  }
}

resource "tls_locally_signed_cert" "leaf" {
  cert_request_pem      = tls_cert_request.leaf.cert_request_pem
  ca_private_key_pem    = tls_private_key.ca.private_key_pem
  ca_cert_pem           = tls_self_signed_cert.ca.cert_pem
  validity_period_hours = 24 * 90
  allowed_uses          = ["digital_signature", "key_encipherment", "server_auth", "client_auth"]
}

resource "google_certificate_manager_certificate" "self" {
  name        = "tf-certs-self"
  description = "self-managed certificate under test"
  labels      = { env = "acceptance" }
  self_managed {
    pem_certificate = tls_locally_signed_cert.leaf.cert_pem
    pem_private_key = tls_private_key.leaf.private_key_pem
  }
}

resource "google_certificate_manager_certificate" "client" {
  name  = "tf-certs-client"
  scope = "CLIENT_AUTH"
  self_managed {
    pem_certificate = tls_locally_signed_cert.leaf.cert_pem
    pem_private_key = tls_private_key.leaf.private_key_pem
  }
}

resource "google_certificate_manager_dns_authorization" "a" {
  name   = "tf-certs-dnsauth"
  domain = "managed.example.test"
}

resource "google_certificate_manager_certificate" "managed" {
  name = "tf-certs-managed"
  managed {
    domains            = [google_certificate_manager_dns_authorization.a.domain]
    dns_authorizations = [google_certificate_manager_dns_authorization.a.id]
  }
}

resource "google_certificate_manager_certificate_map" "m" {
  name        = "tf-certs-map"
  description = "map under test"
}

resource "google_certificate_manager_certificate_map_entry" "host" {
  name         = "tf-certs-entry"
  map          = google_certificate_manager_certificate_map.m.name
  hostname     = "certs.example.test"
  certificates = [google_certificate_manager_certificate.self.id]
}

resource "google_certificate_manager_certificate_map_entry" "primary" {
  name         = "tf-certs-primary"
  map          = google_certificate_manager_certificate_map.m.name
  matcher      = "PRIMARY"
  certificates = [google_certificate_manager_certificate.managed.id]
}

resource "google_certificate_manager_trust_config" "t" {
  name        = "tf-certs-trust"
  location    = "global"
  description = "trust config under test"
  trust_stores {
    trust_anchors {
      pem_certificate = tls_self_signed_cert.ca.cert_pem
    }
  }
}

resource "google_network_security_backend_authentication_config" "b" {
  name               = "tf-certs-backend"
  location           = "global"
  client_certificate = google_certificate_manager_certificate.client.id
  trust_config       = google_certificate_manager_trust_config.t.id
  well_known_roots   = "NONE"
}

resource "google_network_security_server_tls_policy" "s" {
  name        = "tf-certs-server"
  location    = "global"
  description = "server TLS policy under test"
  mtls_policy {
    client_validation_mode         = "REJECT_INVALID"
    client_validation_trust_config = "projects/${var.project}/locations/global/trustConfigs/${google_certificate_manager_trust_config.t.name}"
  }
}

resource "google_network_security_client_tls_policy" "c" {
  name        = "tf-certs-client"
  location    = "global"
  description = "client TLS policy under test"
  sni         = "certs.example.test"
}
