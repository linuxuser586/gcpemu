# VPC networking: networks, subnets, firewalls, routes, addresses, Cloud
# Router with NAT, zonal and regional NEGs with endpoints, private services
# access and VPC Network Peering.

resource "google_compute_network" "vpc" {
  name                    = "tf-net-vpc"
  auto_create_subnetworks = false
  routing_mode            = "GLOBAL"
  mtu                     = 1460
  description             = "network under test"
}

resource "google_compute_network" "auto" {
  name = "tf-net-auto"
}

resource "google_compute_subnetwork" "s" {
  name                     = "tf-net-subnet"
  region                   = var.region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = "10.40.0.0/20"
  private_ip_google_access = true
  description              = "subnet under test"
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.41.0.0/16"
  }
  log_config {
    aggregation_interval = "INTERVAL_10_MIN"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

resource "google_compute_firewall" "allow" {
  name          = "tf-net-allow"
  network       = google_compute_network.vpc.id
  direction     = "INGRESS"
  priority      = 900
  source_ranges = ["10.0.0.0/8"]
  target_tags   = ["web"]
  allow {
    protocol = "tcp"
    ports    = ["80", "443", "8000-8100"]
  }
  allow {
    protocol = "icmp"
  }
}

resource "google_compute_firewall" "deny" {
  name               = "tf-net-deny"
  network            = google_compute_network.vpc.id
  direction          = "EGRESS"
  destination_ranges = ["192.0.2.0/24"]
  deny {
    protocol = "all"
  }
  log_config {
    metadata = "EXCLUDE_ALL_METADATA"
  }
}

resource "google_compute_route" "r" {
  name             = "tf-net-route"
  network          = google_compute_network.vpc.id
  dest_range       = "198.51.100.0/24"
  next_hop_gateway = "default-internet-gateway"
  priority         = 100
  tags             = ["web"]
}

resource "google_compute_address" "external" {
  name   = "tf-net-ext"
  region = var.region
  labels = { env = "acceptance" }
}

resource "google_compute_address" "internal" {
  name         = "tf-net-int"
  region       = var.region
  address_type = "INTERNAL"
  subnetwork   = google_compute_subnetwork.s.id
  address      = "10.40.0.10"
}

resource "google_compute_global_address" "lb" {
  name = "tf-net-global"
}

resource "google_compute_global_address" "psa" {
  name          = "tf-net-psa"
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

resource "google_compute_router" "r" {
  name    = "tf-net-router"
  region  = var.region
  network = google_compute_network.vpc.id
  bgp {
    asn = 64514
  }
}

resource "google_compute_router_nat" "auto" {
  name                               = "tf-net-nat"
  router                             = google_compute_router.r.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"
  min_ports_per_vm                   = 128
  subnetwork {
    name                    = google_compute_subnetwork.s.id
    source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
  }
  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

resource "google_compute_address" "nat" {
  name   = "tf-net-nat-ip"
  region = var.region
}

resource "google_compute_router" "manual" {
  name    = "tf-net-router-manual"
  region  = var.region
  network = google_compute_network.vpc.id
}

resource "google_compute_router_nat" "manual" {
  name                               = "tf-net-nat-manual"
  router                             = google_compute_router.manual.name
  region                             = var.region
  nat_ip_allocate_option             = "MANUAL_ONLY"
  nat_ips                            = [google_compute_address.nat.self_link]
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"
  subnetwork {
    name                    = google_compute_subnetwork.manual.id
    source_ip_ranges_to_nat = ["PRIMARY_IP_RANGE"]
  }
}

# A subnet range is served by one NAT gateway per network and region.
resource "google_compute_subnetwork" "manual" {
  name          = "tf-net-subnet-manual"
  region        = var.region
  network       = google_compute_network.vpc.id
  ip_cidr_range = "10.42.0.0/24"
}

resource "google_compute_network_endpoint_group" "neg" {
  name                  = "tf-net-neg"
  zone                  = var.zone
  network               = google_compute_network.vpc.id
  subnetwork            = google_compute_subnetwork.s.id
  network_endpoint_type = "GCE_VM_IP_PORT"
  default_port          = 8080
}

resource "google_compute_network_endpoint_group" "neg2" {
  name                  = "tf-net-neg2"
  zone                  = var.zone
  network               = google_compute_network.vpc.id
  subnetwork            = google_compute_subnetwork.s.id
  network_endpoint_type = "NON_GCP_PRIVATE_IP_PORT"
  default_port          = 443
}

resource "google_compute_network_endpoint" "e" {
  network_endpoint_group = google_compute_network_endpoint_group.neg2.name
  zone                   = var.zone
  ip_address             = "10.40.0.20"
  port                   = 443
}

resource "google_compute_network_endpoint_group" "neg3" {
  name                  = "tf-net-neg3"
  zone                  = var.zone
  network               = google_compute_network.vpc.id
  subnetwork            = google_compute_subnetwork.s.id
  network_endpoint_type = "NON_GCP_PRIVATE_IP_PORT"
  default_port          = 80
}

resource "google_compute_network_endpoints" "es" {
  network_endpoint_group = google_compute_network_endpoint_group.neg3.name
  zone                   = var.zone
  network_endpoints {
    ip_address = "10.40.0.30"
    port       = 80
  }
  network_endpoints {
    ip_address = "10.40.0.31"
    port       = 80
  }
}

# VPC Network Peering (recorded): ACTIVE once both sides peer.
resource "google_compute_network" "peer" {
  name                    = "tf-net-peer"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "peer" {
  name          = "tf-net-peer"
  region        = var.region
  network       = google_compute_network.peer.id
  ip_cidr_range = "10.60.0.0/24"
}

resource "google_compute_network_peering" "to_peer" {
  name                 = "tf-net-to-peer"
  network              = google_compute_network.vpc.self_link
  peer_network         = google_compute_network.peer.self_link
  export_custom_routes = true
  depends_on           = [google_compute_subnetwork.peer]
}

resource "google_compute_network_peering" "from_peer" {
  name                 = "tf-net-from-peer"
  network              = google_compute_network.peer.self_link
  peer_network         = google_compute_network.vpc.self_link
  import_custom_routes = true
  depends_on           = [google_compute_network_peering.to_peer]
}

# Regional NEGs: serverless, internet (with an endpoint) and Private
# Service Connect.
resource "google_compute_region_network_endpoint_group" "run" {
  name                  = "tf-net-run"
  region                = var.region
  network_endpoint_type = "SERVERLESS"
  cloud_run {
    service = "hello"
  }
}

resource "google_compute_region_network_endpoint_group" "internet" {
  name                  = "tf-net-inet"
  region                = var.region
  network               = google_compute_network.vpc.id
  network_endpoint_type = "INTERNET_IP_PORT"
}

resource "google_compute_region_network_endpoint" "e" {
  region_network_endpoint_group = google_compute_region_network_endpoint_group.internet.name
  region                        = var.region
  ip_address                    = "203.0.113.10"
  port                          = 443
}

resource "google_compute_region_network_endpoint_group" "psc" {
  name                  = "tf-net-psc"
  region                = var.region
  network_endpoint_type = "PRIVATE_SERVICE_CONNECT"
  psc_target_service    = "${var.region}-cloudkms.googleapis.com"
}
