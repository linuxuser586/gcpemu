# Cloud Load Balancing and Cloud CDN: a global external Application Load
# Balancer (backend bucket with CDN and signed URL keys, backend service on
# a NEG, HTTPS with a self-managed and a Google-managed certificate and an
# SSL policy) and a regional internal one with the regional variants, plus
# the recorded resources: Cloud Armor policies, legacy health checks, TCP,
# SSL and gRPC target proxies and an internet NEG backend.

resource "tls_private_key" "k" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "c" {
  private_key_pem       = tls_private_key.k.private_key_pem
  validity_period_hours = 24 * 30
  dns_names             = ["lb.example.test"]
  allowed_uses          = ["digital_signature", "server_auth"]
  subject {
    common_name = "lb.example.test"
  }
}

resource "google_compute_network" "vpc" {
  name                    = "tf-lb-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "s" {
  name          = "tf-lb-subnet"
  region        = var.region
  network       = google_compute_network.vpc.id
  ip_cidr_range = "10.50.0.0/24"
}

resource "google_compute_subnetwork" "proxy" {
  name          = "tf-lb-proxy"
  region        = var.region
  network       = google_compute_network.vpc.id
  ip_cidr_range = "10.50.1.0/24"
  purpose       = "REGIONAL_MANAGED_PROXY"
  role          = "ACTIVE"
}

resource "google_compute_network_endpoint_group" "neg" {
  name                  = "tf-lb-neg"
  zone                  = var.zone
  network               = google_compute_network.vpc.id
  subnetwork            = google_compute_subnetwork.s.id
  network_endpoint_type = "GCE_VM_IP_PORT"
  default_port          = 8080
}

# ---- Global ----

resource "google_storage_bucket" "static" {
  name                        = "${var.project}-tf-lb"
  location                    = "US"
  uniform_bucket_level_access = true
  force_destroy               = true
}

resource "google_compute_backend_bucket" "static" {
  name        = "tf-lb-static"
  bucket_name = google_storage_bucket.static.name
  description = "backend bucket under test"
  enable_cdn  = true
  cdn_policy {
    cache_mode        = "CACHE_ALL_STATIC"
    default_ttl       = 3600
    client_ttl        = 3600
    max_ttl           = 86400
    negative_caching  = true
    serve_while_stale = 86400
  }
  custom_response_headers = ["X-Cache-Status: {cdn_cache_status}"]
}

resource "google_compute_backend_bucket_signed_url_key" "k" {
  name           = "tf-lb-bucket-key"
  key_value      = "pPsVemX_GDuZ6wtDzc7ExA=="
  backend_bucket = google_compute_backend_bucket.static.name
}

resource "google_compute_health_check" "http" {
  name                = "tf-lb-hc"
  check_interval_sec  = 5
  timeout_sec         = 5
  healthy_threshold   = 2
  unhealthy_threshold = 3
  http_health_check {
    port         = 8080
    request_path = "/healthz"
  }
}

resource "google_compute_health_check" "tcp" {
  name = "tf-lb-hc-tcp"
  tcp_health_check {
    port = 5432
  }
}

resource "google_compute_backend_service" "api" {
  name                  = "tf-lb-api"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTP"
  port_name             = "http"
  timeout_sec           = 30
  health_checks         = [google_compute_health_check.http.id]
  enable_cdn            = true
  cdn_policy {
    cache_mode  = "USE_ORIGIN_HEADERS"
    signed_url_cache_max_age_sec = 600
    cache_key_policy {
      include_host         = true
      include_protocol     = true
      include_query_string = true
    }
  }
  backend {
    group                 = google_compute_network_endpoint_group.neg.id
    balancing_mode        = "RATE"
    max_rate_per_endpoint = 100
  }
  log_config {
    enable      = true
    sample_rate = 0.5
  }
}

resource "google_compute_backend_service_signed_url_key" "k" {
  name            = "tf-lb-service-key"
  key_value       = "iqtXl2mXJu1GfbbYyHZx6w=="
  backend_service = google_compute_backend_service.api.name
}

resource "google_compute_url_map" "web" {
  name            = "tf-lb-web"
  default_service = google_compute_backend_bucket.static.id
  host_rule {
    hosts        = ["lb.example.test"]
    path_matcher = "app"
  }
  path_matcher {
    name            = "app"
    default_service = google_compute_backend_bucket.static.id
    path_rule {
      paths   = ["/api", "/api/*"]
      service = google_compute_backend_service.api.id
    }
  }
}

resource "google_compute_ssl_certificate" "self" {
  name        = "tf-lb-cert"
  private_key = tls_private_key.k.private_key_pem
  certificate = tls_self_signed_cert.c.cert_pem
}

resource "google_compute_managed_ssl_certificate" "managed" {
  name = "tf-lb-managed"
  managed {
    domains = ["lb.example.test"]
  }
}

resource "google_compute_ssl_policy" "modern" {
  name            = "tf-lb-modern"
  profile         = "MODERN"
  min_tls_version = "TLS_1_2"
}

resource "google_compute_target_https_proxy" "web" {
  name             = "tf-lb-https"
  url_map          = google_compute_url_map.web.id
  ssl_certificates = [google_compute_ssl_certificate.self.id, google_compute_managed_ssl_certificate.managed.id]
  ssl_policy       = google_compute_ssl_policy.modern.id
}

resource "google_compute_url_map" "redirect" {
  name = "tf-lb-redirect"
  default_url_redirect {
    https_redirect         = true
    redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
    strip_query            = false
  }
}

resource "google_compute_target_http_proxy" "redirect" {
  name    = "tf-lb-http"
  url_map = google_compute_url_map.redirect.id
}

resource "google_compute_global_address" "lb" {
  name = "tf-lb-ip"
}

resource "google_compute_global_forwarding_rule" "https" {
  name                  = "tf-lb-https"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_address            = google_compute_global_address.lb.id
  port_range            = "443"
  target                = google_compute_target_https_proxy.web.id
  labels                = { env = "acceptance" }
}

resource "google_compute_global_forwarding_rule" "http" {
  name                  = "tf-lb-http"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_address            = google_compute_global_address.lb.id
  port_range            = "80"
  target                = google_compute_target_http_proxy.redirect.id
}

# ---- Regional internal ----

resource "google_compute_region_health_check" "http" {
  name   = "tf-lb-rhc"
  region = var.region
  http_health_check {
    port         = 8080
    request_path = "/healthz"
  }
}

resource "google_compute_region_backend_service" "api" {
  name                  = "tf-lb-rapi"
  region                = var.region
  load_balancing_scheme = "INTERNAL_MANAGED"
  protocol              = "HTTP"
  health_checks         = [google_compute_region_health_check.http.id]
  backend {
    group                 = google_compute_network_endpoint_group.neg.id
    balancing_mode        = "RATE"
    max_rate_per_endpoint = 50
    capacity_scaler       = 1
  }
}

resource "google_compute_region_url_map" "web" {
  name            = "tf-lb-rweb"
  region          = var.region
  default_service = google_compute_region_backend_service.api.id
}

resource "google_compute_region_ssl_certificate" "self" {
  name        = "tf-lb-rcert"
  region      = var.region
  private_key = tls_private_key.k.private_key_pem
  certificate = tls_self_signed_cert.c.cert_pem
}

resource "google_compute_region_ssl_policy" "compat" {
  name            = "tf-lb-rpolicy"
  region          = var.region
  profile         = "CUSTOM"
  min_tls_version = "TLS_1_2"
  custom_features = ["TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"]
}

resource "google_compute_region_target_https_proxy" "web" {
  name             = "tf-lb-rhttps"
  region           = var.region
  url_map          = google_compute_region_url_map.web.id
  ssl_certificates = [google_compute_region_ssl_certificate.self.id]
  ssl_policy       = google_compute_region_ssl_policy.compat.id
}

resource "google_compute_region_target_http_proxy" "web" {
  name    = "tf-lb-rhttp"
  region  = var.region
  url_map = google_compute_region_url_map.web.id
}

resource "google_compute_forwarding_rule" "https" {
  name                  = "tf-lb-rhttps"
  region                = var.region
  load_balancing_scheme = "INTERNAL_MANAGED"
  network               = google_compute_network.vpc.id
  subnetwork            = google_compute_subnetwork.s.id
  port_range            = "443"
  target                = google_compute_region_target_https_proxy.web.id
  depends_on            = [google_compute_subnetwork.proxy]
}

resource "google_compute_forwarding_rule" "http" {
  name                  = "tf-lb-rhttp"
  region                = var.region
  load_balancing_scheme = "INTERNAL_MANAGED"
  network               = google_compute_network.vpc.id
  subnetwork            = google_compute_subnetwork.s.id
  port_range            = "80"
  target                = google_compute_region_target_http_proxy.web.id
  depends_on            = [google_compute_subnetwork.proxy]
}

# ---- Cloud Armor (recorded) ----

resource "google_compute_security_policy" "armor" {
  name        = "tf-lb-armor"
  description = "backend policy under test"
  rule {
    action      = "deny(403)"
    priority    = 1000
    description = "block TEST-NET-1"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["192.0.2.0/24"]
      }
    }
  }
  rule {
    action   = "allow"
    priority = 2000
    match {
      expr {
        expression = "request.path.matches('/api')"
      }
    }
  }
  rule {
    action      = "allow"
    priority    = 2147483647
    description = "default rule"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
  }
}

resource "google_compute_security_policy" "edge" {
  name = "tf-lb-edge"
  type = "CLOUD_ARMOR_EDGE"
}

resource "google_compute_backend_service" "armored" {
  name                  = "tf-lb-armored"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTP"
  health_checks         = [google_compute_health_check.http.id]
  security_policy       = google_compute_security_policy.armor.id
  edge_security_policy  = google_compute_security_policy.edge.id
  backend {
    group                 = google_compute_network_endpoint_group.neg.id
    balancing_mode        = "RATE"
    max_rate_per_endpoint = 100
  }
}

# ---- Legacy health checks (recorded; target pools use them) ----

resource "google_compute_http_health_check" "legacy" {
  name         = "tf-lb-legacy-http"
  request_path = "/healthz"
  port         = 8080
}

resource "google_compute_https_health_check" "legacy" {
  name               = "tf-lb-legacy-https"
  check_interval_sec = 10
  timeout_sec        = 5
}

# ---- Proxy Network Load Balancers and proxyless gRPC (recorded) ----

resource "google_compute_network_endpoint_group" "l4" {
  name                  = "tf-lb-neg-l4"
  zone                  = var.zone
  network               = google_compute_network.vpc.id
  subnetwork            = google_compute_subnetwork.s.id
  network_endpoint_type = "GCE_VM_IP_PORT"
  default_port          = 5432
}

resource "google_compute_backend_service" "tcp" {
  name                  = "tf-lb-tcp"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "TCP"
  health_checks         = [google_compute_health_check.tcp.id]
  backend {
    group                        = google_compute_network_endpoint_group.l4.id
    balancing_mode               = "CONNECTION"
    max_connections_per_endpoint = 100
  }
}

resource "google_compute_target_tcp_proxy" "tcp" {
  name            = "tf-lb-tcp"
  backend_service = google_compute_backend_service.tcp.id
  proxy_header    = "PROXY_V1"
}

resource "google_compute_global_forwarding_rule" "tcp" {
  name                  = "tf-lb-tcp"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  port_range            = "5432"
  target                = google_compute_target_tcp_proxy.tcp.id
}

resource "google_compute_backend_service" "ssl" {
  name                  = "tf-lb-ssl"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "SSL"
  health_checks         = [google_compute_health_check.tcp.id]
  backend {
    group                        = google_compute_network_endpoint_group.l4.id
    balancing_mode               = "CONNECTION"
    max_connections_per_endpoint = 100
  }
}

resource "google_compute_target_ssl_proxy" "ssl" {
  name             = "tf-lb-ssl"
  backend_service  = google_compute_backend_service.ssl.id
  ssl_certificates = [google_compute_ssl_certificate.self.id]
  ssl_policy       = google_compute_ssl_policy.modern.id
}

resource "google_compute_health_check" "grpc" {
  name = "tf-lb-hc-grpc"
  grpc_health_check {
    port = 8443
  }
}

resource "google_compute_backend_service" "grpc" {
  name                  = "tf-lb-grpc"
  load_balancing_scheme = "INTERNAL_SELF_MANAGED"
  protocol              = "GRPC"
  health_checks         = [google_compute_health_check.grpc.id]
}

resource "google_compute_url_map" "grpc" {
  name            = "tf-lb-grpc"
  default_service = google_compute_backend_service.grpc.id
}

resource "google_compute_target_grpc_proxy" "grpc" {
  name                   = "tf-lb-grpc"
  url_map                = google_compute_url_map.grpc.id
  validate_for_proxyless = true
}

resource "google_compute_region_target_tcp_proxy" "regional" {
  name            = "tf-lb-rtcp"
  region          = var.region
  backend_service = google_compute_region_backend_service.tcp.id
}

resource "google_compute_region_backend_service" "tcp" {
  name                  = "tf-lb-rtcp"
  region                = var.region
  load_balancing_scheme = "INTERNAL_MANAGED"
  protocol              = "TCP"
  health_checks         = [google_compute_region_health_check.http.id]
}

# ---- Internet NEG backend ----

resource "google_compute_global_network_endpoint_group" "ext" {
  name                  = "tf-lb-ext"
  network_endpoint_type = "INTERNET_FQDN_PORT"
  default_port          = 443
}

resource "google_compute_global_network_endpoint" "ext" {
  global_network_endpoint_group = google_compute_global_network_endpoint_group.ext.name
  fqdn                          = "origin.example.test"
  port                          = 443
}

resource "google_compute_backend_service" "ext" {
  name                  = "tf-lb-ext"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTPS"
  backend {
    group = google_compute_global_network_endpoint_group.ext.id
  }
}
