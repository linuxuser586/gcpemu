# Global external Application Load Balancer (EXTERNAL_MANAGED) for
# https://app.<domain>: static paths from a CDN-enabled backend bucket,
# /api/* to the Istio ingress gateway's GKE NEG over HTTPS with backend
# mTLS. Every response carries X-Cache-Status: {cdn_cache_status}.

# ---- Static assets ----

resource "google_storage_bucket" "static" {
  name                        = "${var.project}-static"
  location                    = "US"
  uniform_bucket_level_access = true
  force_destroy               = true
  website {
    main_page_suffix = "index.html"
    not_found_page   = "404.html"
  }
}

resource "google_storage_bucket_iam_member" "static_public" {
  bucket = google_storage_bucket.static.name
  role   = "roles/storage.objectViewer"
  member = "allUsers"
}

resource "google_storage_bucket_object" "assets" {
  for_each      = { for f in fileset("${path.module}/assets", "*") : f => f }
  bucket        = google_storage_bucket.static.name
  name          = each.value
  source        = "${path.module}/assets/${each.value}"
  content_type  = lookup({ html = "text/html; charset=utf-8", css = "text/css", svg = "image/svg+xml" }, reverse(split(".", each.value))[0], "application/octet-stream")
  cache_control = "public, max-age=3600"
}

resource "google_compute_backend_bucket" "static" {
  name        = "ref-static"
  bucket_name = google_storage_bucket.static.name
  enable_cdn  = true
  cdn_policy {
    cache_mode  = "CACHE_ALL_STATIC"
    default_ttl = 3600
    client_ttl  = 3600
    max_ttl     = 86400
  }
}

# ---- API backend: Istio ingress gateway NEG with backend mTLS ----

data "google_compute_network_endpoint_group" "gateway" {
  count = var.api_neg_name != "" ? 1 : 0
  name  = var.api_neg_name
  zone  = var.zone
}

resource "google_compute_health_check" "gateway" {
  name                = "ref-gateway"
  check_interval_sec  = 2
  timeout_sec         = 2
  healthy_threshold   = 1
  unhealthy_threshold = 2
  http_health_check {
    port         = 15021
    request_path = "/healthz/ready"
  }
}

resource "google_compute_backend_service" "api" {
  count                 = var.api_neg_name != "" ? 1 : 0
  name                  = "ref-api"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTPS"
  timeout_sec           = 30
  health_checks         = [google_compute_health_check.gateway.id]

  backend {
    group                 = data.google_compute_network_endpoint_group.gateway[0].id
    balancing_mode        = "RATE"
    max_rate_per_endpoint = 100
  }

  tls_settings {
    sni                   = local.gateway_name
    authentication_config = "//networksecurity.googleapis.com/${google_network_security_backend_authentication_config.lb.id}"
  }

  log_config {
    enable      = true
    sample_rate = 1
  }
}

# ---- Routing ----

resource "google_compute_url_map" "web" {
  name            = "ref-web"
  default_service = google_compute_backend_bucket.static.id

  header_action {
    response_headers_to_add {
      header_name  = "X-Cache-Status"
      header_value = "{cdn_cache_status}"
      replace      = true
    }
  }

  host_rule {
    hosts        = [local.app_host]
    path_matcher = "app"
  }

  path_matcher {
    name            = "app"
    default_service = google_compute_backend_bucket.static.id

    dynamic "path_rule" {
      for_each = google_compute_backend_service.api
      content {
        paths   = ["/api", "/api/*"]
        service = path_rule.value.id
      }
    }
  }
}

resource "google_compute_global_address" "lb" {
  name = "ref-lb"
}

resource "google_compute_managed_ssl_certificate" "app" {
  name = "ref-app"
  managed {
    domains = [local.app_host]
  }
}

resource "google_compute_target_https_proxy" "web" {
  name             = "ref-web"
  url_map          = google_compute_url_map.web.id
  ssl_certificates = [google_compute_managed_ssl_certificate.app.id]
}

resource "google_compute_global_forwarding_rule" "https" {
  name                  = "ref-https"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_address            = google_compute_global_address.lb.id
  port_range            = "443"
  target                = google_compute_target_https_proxy.web.id
}

# HTTP → HTTPS redirect on the same address.
resource "google_compute_url_map" "redirect" {
  name = "ref-redirect"
  default_url_redirect {
    https_redirect         = true
    redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
    strip_query            = false
  }
}

resource "google_compute_target_http_proxy" "redirect" {
  name    = "ref-redirect"
  url_map = google_compute_url_map.redirect.id
}

resource "google_compute_global_forwarding_rule" "http" {
  name                  = "ref-http"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_address            = google_compute_global_address.lb.id
  port_range            = "80"
  target                = google_compute_target_http_proxy.redirect.id
}

# ---- DNS ----

resource "google_dns_managed_zone" "example" {
  name     = "example"
  dns_name = var.domain
}

resource "google_dns_record_set" "app" {
  managed_zone = google_dns_managed_zone.example.name
  name         = "${local.app_host}."
  type         = "A"
  ttl          = 60
  rrdatas      = [google_compute_global_address.lb.address]
}
