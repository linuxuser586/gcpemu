# Cloud DNS: a public and a private zone and record sets of the common
# types, including a routing policy.

resource "google_dns_managed_zone" "public" {
  name        = "tf-public"
  dns_name    = "tf-public.test."
  description = "public zone under test"
  labels      = { env = "acceptance" }
}

resource "google_compute_network" "dns" {
  name                    = "tf-dns-vpc"
  auto_create_subnetworks = false
}

resource "google_dns_managed_zone" "private" {
  name       = "tf-private"
  dns_name   = "tf-private.internal."
  visibility = "private"
  private_visibility_config {
    networks {
      network_url = google_compute_network.dns.id
    }
  }
}

resource "google_dns_record_set" "a" {
  managed_zone = google_dns_managed_zone.public.name
  name         = "www.${google_dns_managed_zone.public.dns_name}"
  type         = "A"
  ttl          = 300
  rrdatas      = ["192.0.2.10", "192.0.2.11"]
}

resource "google_dns_record_set" "cname" {
  managed_zone = google_dns_managed_zone.public.name
  name         = "alias.${google_dns_managed_zone.public.dns_name}"
  type         = "CNAME"
  ttl          = 60
  rrdatas      = ["www.${google_dns_managed_zone.public.dns_name}"]
}

resource "google_dns_record_set" "txt" {
  managed_zone = google_dns_managed_zone.public.name
  name         = google_dns_managed_zone.public.dns_name
  type         = "TXT"
  ttl          = 300
  rrdatas      = ["\"v=spf1 -all\""]
}

resource "google_dns_record_set" "mx" {
  managed_zone = google_dns_managed_zone.public.name
  name         = google_dns_managed_zone.public.dns_name
  type         = "MX"
  ttl          = 3600
  rrdatas      = ["10 mx1.example.test.", "20 mx2.example.test."]
}

resource "google_dns_record_set" "geo" {
  managed_zone = google_dns_managed_zone.public.name
  name         = "geo.${google_dns_managed_zone.public.dns_name}"
  type         = "A"
  ttl          = 60
  routing_policy {
    wrr {
      weight  = 0.8
      rrdatas = ["192.0.2.20"]
    }
    wrr {
      weight  = 0.2
      rrdatas = ["192.0.2.21"]
    }
  }
}

resource "google_dns_record_set" "internal" {
  managed_zone = google_dns_managed_zone.private.name
  name         = "db.${google_dns_managed_zone.private.dns_name}"
  type         = "A"
  ttl          = 30
  rrdatas      = ["10.0.0.5"]
}
