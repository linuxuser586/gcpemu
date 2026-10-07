# GKE Standard: a VPC-native private cluster with Workload Identity and a
# separately managed node pool with autoscaling.

resource "google_compute_network" "vpc" {
  name                    = "tf-gke-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "nodes" {
  name          = "tf-gke-nodes"
  region        = var.region
  network       = google_compute_network.vpc.id
  ip_cidr_range = "10.70.0.0/20"
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.71.0.0/16"
  }
  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.72.0.0/20"
  }
}

resource "google_service_account" "nodes" {
  account_id = "tf-gke-nodes"
}

resource "google_container_cluster" "c" {
  name                     = "tf-gke"
  location                 = var.zone
  network                  = google_compute_network.vpc.id
  subnetwork               = google_compute_subnetwork.nodes.id
  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = false
  resource_labels          = { env = "acceptance" }

  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = "172.16.0.16/28"
  }

  workload_identity_config {
    workload_pool = "${var.project}.svc.id.goog"
  }

  release_channel {
    channel = "REGULAR"
  }
}

resource "google_container_node_pool" "p" {
  name     = "pool"
  cluster  = google_container_cluster.c.id
  location = var.zone

  autoscaling {
    min_node_count = 1
    max_node_count = 2
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type    = "e2-standard-4"
    disk_size_gb    = 50
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { pool = "p" }
    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}

# The node pool's instance group, as GKE NEG-less Ingress setups name it.
resource "google_compute_instance_group_named_port" "http" {
  group = google_container_node_pool.p.managed_instance_group_urls[0]
  zone  = var.zone
  name  = "http"
  port  = 30080
}
