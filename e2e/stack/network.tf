# VPC, subnet with secondary ranges for pods and services, Cloud Router +
# NAT for the private nodes, private services access for Cloud SQL.

resource "google_compute_network" "vpc" {
  name                    = "ref-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "nodes" {
  name                     = "ref-nodes"
  region                   = var.region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = "10.10.0.0/20"
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.20.0.0/16"
  }
  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.30.0.0/20"
  }
}

resource "google_compute_router" "router" {
  name    = "ref-router"
  region  = var.region
  network = google_compute_network.vpc.id
}

resource "google_compute_router_nat" "nat" {
  count                              = var.nat_enabled ? 1 : 0
  name                               = "ref-nat"
  router                             = google_compute_router.router.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}

# Google's load balancer and health check ranges may reach the pods (the
# NEG endpoints of the Istio ingress gateway).
resource "google_compute_firewall" "lb_to_gateway" {
  name          = "ref-allow-lb"
  network       = google_compute_network.vpc.id
  direction     = "INGRESS"
  source_ranges = ["130.211.0.0/22", "35.191.0.0/16"]
  allow {
    protocol = "tcp"
    ports    = ["8443", "15021"]
  }
}

resource "google_compute_global_address" "psa" {
  name          = "ref-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = 20
  network       = google_compute_network.vpc.id
}

resource "google_service_networking_connection" "psa" {
  network                 = google_compute_network.vpc.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.psa.name]
}
