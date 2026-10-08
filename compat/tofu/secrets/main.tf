# Secret Manager: global and regional secrets with versions, aliases,
# rotation notifications, expiry, delayed destruction and secret IAM
# policy, read back through the version data source.

resource "google_pubsub_topic" "events" {
  name = "tf-secret-events"
}

resource "google_secret_manager_secret" "db" {
  secret_id = "tf-db-password"
  labels    = { env = "acceptance" }
  annotations = {
    owner = "platform"
  }
  replication {
    auto {}
  }
  topics {
    name = google_pubsub_topic.events.id
  }
  rotation {
    next_rotation_time = timeadd(plantimestamp(), "720h")
    rotation_period    = "2592000s"
  }
  version_destroy_ttl = "86400s"
  lifecycle {
    ignore_changes = [rotation[0].next_rotation_time]
  }
}

resource "google_secret_manager_secret_version" "db" {
  secret      = google_secret_manager_secret.db.id
  secret_data = "s3cret"
}

resource "google_secret_manager_secret" "replicated" {
  secret_id = "tf-replicated"
  ttl       = "86400s"
  replication {
    user_managed {
      replicas {
        location = "us-central1"
      }
      replicas {
        location = "us-east1"
      }
    }
  }
}

resource "google_secret_manager_secret_version" "replicated" {
  secret      = google_secret_manager_secret.replicated.id
  secret_data = "replicated"
  enabled     = false
}

resource "google_service_account" "reader" {
  account_id = "tf-secret-reader"
}

resource "google_secret_manager_secret_iam_member" "reader" {
  secret_id = google_secret_manager_secret.db.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.reader.email}"
}

resource "google_secret_manager_secret_iam_binding" "viewers" {
  secret_id = google_secret_manager_secret.replicated.id
  role      = "roles/secretmanager.viewer"
  members   = ["serviceAccount:${google_service_account.reader.email}"]
}

resource "google_secret_manager_secret" "policy" {
  secret_id = "tf-policy"
  replication {
    auto {}
  }
}

data "google_iam_policy" "adders" {
  binding {
    role    = "roles/secretmanager.secretVersionAdder"
    members = ["serviceAccount:${google_service_account.reader.email}"]
  }
}

resource "google_secret_manager_secret_iam_policy" "policy" {
  secret_id   = google_secret_manager_secret.policy.id
  policy_data = data.google_iam_policy.adders.policy_data
}

resource "google_secret_manager_regional_secret" "regional" {
  secret_id = "tf-regional"
  location  = var.region
  labels    = { env = "acceptance" }
  version_aliases = {}
}

resource "google_secret_manager_regional_secret_version" "regional" {
  secret      = google_secret_manager_regional_secret.regional.id
  secret_data = "regional"
}

data "google_secret_manager_secret_version" "db" {
  secret     = google_secret_manager_secret.db.secret_id
  depends_on = [google_secret_manager_secret_version.db]
}

output "db_password" {
  value     = data.google_secret_manager_secret_version.db.secret_data
  sensitive = true
}
