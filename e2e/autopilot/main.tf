# GKE Autopilot cluster on the Rapid channel, which offers every supported
# minor, with NET_ADMIN allowed for Istio's sidecar injection
# (workloadPolicyConfig.allowNetAdmin). Its nodes are public, so pods pull
# images from the internet without Cloud NAT.

resource "google_compute_network" "vpc" {
  name                    = "ap-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "nodes" {
  name          = "ap-nodes"
  region        = var.region
  network       = google_compute_network.vpc.id
  ip_cidr_range = "10.90.0.0/20"
}

resource "google_container_cluster" "ap" {
  name                = "ap"
  location            = var.region
  network             = google_compute_network.vpc.id
  subnetwork          = google_compute_subnetwork.nodes.id
  enable_autopilot    = true
  allow_net_admin     = true
  min_master_version  = var.kubernetes_version != "" ? var.kubernetes_version : null
  deletion_protection = false

  release_channel {
    channel = "RAPID"
  }
}
