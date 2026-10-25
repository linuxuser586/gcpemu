# GKE Autopilot: a regional private cluster whose settings GKE sets
# (FR-GKE-012).

resource "google_compute_network" "vpc" {
  name                    = "tf-ap-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "nodes" {
  name          = "tf-ap-nodes"
  region        = var.region
  network       = google_compute_network.vpc.id
  ip_cidr_range = "10.80.0.0/20"
}

resource "google_container_cluster" "ap" {
  name                = "tf-ap"
  location            = var.region
  network             = google_compute_network.vpc.id
  subnetwork          = google_compute_subnetwork.nodes.id
  enable_autopilot    = true
  deletion_protection = false

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = "172.16.0.32/28"
  }
}
