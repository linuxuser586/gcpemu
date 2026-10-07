# Artifact Registry: Docker repositories (standard, with cleanup policies,
# and a Docker Hub remote) and their IAM policy.

resource "google_artifact_registry_repository" "docker" {
  location      = var.region
  repository_id = "tf-docker"
  format        = "DOCKER"
  description   = "repository under test"
  labels        = { env = "acceptance" }
  docker_config {
    immutable_tags = true
  }
}

resource "google_artifact_registry_repository" "cleanup" {
  location               = var.region
  repository_id          = "tf-cleanup"
  format                 = "DOCKER"
  cleanup_policy_dry_run = true
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = 5
    }
  }
  cleanup_policies {
    id     = "delete-old"
    action = "DELETE"
    condition {
      tag_state  = "UNTAGGED"
      older_than = "2592000s"
    }
  }
}

resource "google_artifact_registry_repository" "hub" {
  location      = var.region
  repository_id = "tf-hub"
  format        = "DOCKER"
  mode          = "REMOTE_REPOSITORY"
  remote_repository_config {
    description = "Docker Hub"
    docker_repository {
      public_repository = "DOCKER_HUB"
    }
  }
}

resource "google_service_account" "reader" {
  account_id = "tf-ar-reader"
}

resource "google_artifact_registry_repository_iam_member" "docker" {
  location   = google_artifact_registry_repository.docker.location
  repository = google_artifact_registry_repository.docker.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.reader.email}"
}

resource "google_artifact_registry_repository_iam_binding" "cleanup" {
  location   = google_artifact_registry_repository.cleanup.location
  repository = google_artifact_registry_repository.cleanup.name
  role       = "roles/artifactregistry.writer"
  members    = ["serviceAccount:${google_service_account.reader.email}"]
}

data "google_iam_policy" "hub" {
  binding {
    role    = "roles/artifactregistry.reader"
    members = ["serviceAccount:${google_service_account.reader.email}"]
  }
}

resource "google_artifact_registry_repository_iam_policy" "hub" {
  location    = google_artifact_registry_repository.hub.location
  repository  = google_artifact_registry_repository.hub.name
  policy_data = data.google_iam_policy.hub.policy_data
}
