# Private GKE cluster with Workload Identity, the Gateway API CRDs, the
# Secret Manager add-on and secret synchronization, its node pool and the
# Artifact Registry repository the app image is pushed to.

resource "google_service_account" "nodes" {
  account_id   = "ref-nodes"
  display_name = "GKE nodes"
}

resource "google_container_cluster" "gke" {
  name                     = "ref-gke"
  location                 = var.zone
  network                  = google_compute_network.vpc.id
  subnetwork               = google_compute_subnetwork.nodes.id
  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = false

  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = "172.16.0.0/28"
  }

  workload_identity_config {
    workload_pool = "${var.project}.svc.id.goog"
  }

  gateway_api_config {
    channel = "CHANNEL_STANDARD"
  }

  secret_manager_config {
    enabled = true
  }

  secret_sync_config {
    enabled = true
  }
}

resource "google_container_node_pool" "default" {
  name       = "default"
  cluster    = google_container_cluster.gke.id
  location   = var.zone
  node_count = var.node_count

  node_config {
    machine_type    = "e2-standard-4"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}

resource "google_artifact_registry_repository" "app" {
  location      = var.region
  repository_id = "app"
  format        = "DOCKER"
}

# Nodes pull the app image as the node service account (FR-INT-006).
resource "google_artifact_registry_repository_iam_member" "nodes_read" {
  location   = google_artifact_registry_repository.app.location
  repository = google_artifact_registry_repository.app.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.nodes.email}"
}
