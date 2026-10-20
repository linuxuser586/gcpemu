# Cloud Build: Build triggers fired by Pub/Sub and by a webhook, a
# GitHub Build trigger (Recorded, never fired) and a worker pool
# (Recorded).

resource "google_pubsub_topic" "releases" {
  name = "tf-build-releases"
}

resource "google_cloudbuild_trigger" "pubsub" {
  name        = "tf-pubsub"
  description = "Builds each release message."
  tags        = ["acceptance"]

  pubsub_config {
    topic = google_pubsub_topic.releases.id
  }

  source_to_build {
    uri       = "https://github.com/octocat/Hello-World"
    ref       = "refs/heads/master"
    repo_type = "GITHUB"
  }

  substitutions = {
    _TAG = "$(body.message.data)"
  }

  build {
    step {
      name = "busybox"
      args = ["echo", "$_TAG"]
    }
    timeout = "600s"
  }
}

resource "google_secret_manager_secret" "hook" {
  secret_id = "tf-build-hook"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "hook" {
  secret      = google_secret_manager_secret.hook.id
  secret_data = "letmein"
}

resource "google_cloudbuild_trigger" "webhook" {
  name     = "tf-webhook"
  location = var.region

  webhook_config {
    secret = google_secret_manager_secret_version.hook.id
  }

  source_to_build {
    uri       = "https://github.com/octocat/Hello-World"
    ref       = "refs/heads/master"
    repo_type = "GITHUB"
  }

  git_file_source {
    path      = "cloudbuild.yaml"
    uri       = "https://github.com/octocat/Hello-World"
    revision  = "refs/heads/master"
    repo_type = "GITHUB"
  }

  substitutions = {
    _REF = "$(body.ref)"
  }
}

resource "google_cloudbuild_trigger" "github" {
  name     = "tf-github"
  filename = "cloudbuild.yaml"
  disabled = true

  github {
    owner = "octocat"
    name  = "Hello-World"
    push {
      branch = "^master$"
    }
  }

  included_files = ["src/**"]
  ignored_files  = ["docs/**"]
}

resource "google_cloudbuild_worker_pool" "pool" {
  name         = "tf-pool"
  location     = var.region
  display_name = "Acceptance pool"
  annotations = {
    team = "platform"
  }

  worker_config {
    disk_size_gb   = 100
    machine_type   = "e2-standard-4"
    no_external_ip = false
  }
}
