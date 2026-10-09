# What the app uses: its Google service account (bound to the Kubernetes
# service account app/app through Workload Identity), the uploads bucket
# with a Pub/Sub notification, the events topic with a push subscription
# back to the app through the load balancer, and Cloud SQL for
# PostgreSQL 16 on a private IP with an IAM service account user.

locals {
  app_host   = "app.${trimsuffix(var.domain, ".")}"
  app_member = "serviceAccount:${google_service_account.app.email}"
}

resource "google_service_account" "app" {
  account_id   = "ref-app"
  display_name = "Reference stack app"
}

resource "google_service_account_iam_member" "app_wi" {
  service_account_id = google_service_account.app.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project}.svc.id.goog[app/app]"
}

# ---- Cloud Storage ----

resource "google_storage_bucket" "uploads" {
  name                        = "${var.project}-uploads"
  location                    = "US"
  uniform_bucket_level_access = true
  force_destroy               = true
}

resource "google_storage_bucket_iam_member" "app_objects" {
  count  = var.app_storage_access ? 1 : 0
  bucket = google_storage_bucket.uploads.name
  role   = "roles/storage.objectAdmin"
  member = local.app_member
}

data "google_storage_project_service_account" "gcs" {}

resource "google_pubsub_topic_iam_member" "gcs_publish" {
  topic  = google_pubsub_topic.events.id
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:${data.google_storage_project_service_account.gcs.email_address}"
}

resource "google_storage_notification" "uploads" {
  bucket         = google_storage_bucket.uploads.name
  topic          = google_pubsub_topic.events.id
  payload_format = "JSON_API_V1"
  event_types    = ["OBJECT_FINALIZE"]
  depends_on     = [google_pubsub_topic_iam_member.gcs_publish]
}

# ---- Pub/Sub ----

resource "google_pubsub_topic" "events" {
  name = "events"
}

resource "google_pubsub_topic_iam_member" "app_publish" {
  topic  = google_pubsub_topic.events.id
  role   = "roles/pubsub.publisher"
  member = local.app_member
}

resource "google_service_account" "push" {
  account_id   = "ref-push"
  display_name = "Pub/Sub push invoker"
}

resource "google_pubsub_subscription" "push" {
  name                 = "events-push"
  topic                = google_pubsub_topic.events.id
  ack_deadline_seconds = 20
  push_config {
    push_endpoint = "https://${local.app_host}/api/push"
    oidc_token {
      service_account_email = google_service_account.push.email
      audience              = "https://${local.app_host}/api/push"
    }
  }
  retry_policy {
    minimum_backoff = "1s"
    maximum_backoff = "10s"
  }
}

# ---- Cloud SQL ----

resource "google_sql_database_instance" "db" {
  name                = "ref-db"
  database_version    = "POSTGRES_16"
  region              = var.region
  deletion_protection = false
  depends_on          = [google_service_networking_connection.psa]

  settings {
    tier    = "db-custom-1-3840"
    edition = "ENTERPRISE"
    ip_configuration {
      ipv4_enabled    = false
      private_network = google_compute_network.vpc.id
    }
    database_flags {
      name  = "cloudsql.iam_authentication"
      value = "on"
    }
  }
}

resource "google_sql_database" "app" {
  name     = "app"
  instance = google_sql_database_instance.db.name
}

# Postgres roles that own objects or hold grants cannot be dropped through
# the API (like on Cloud SQL); they go away with the instance.
resource "google_sql_user" "admin" {
  name            = "admin"
  instance        = google_sql_database_instance.db.name
  password        = var.db_admin_password
  deletion_policy = "ABANDON"
}

resource "google_sql_user" "app" {
  name            = trimsuffix(google_service_account.app.email, ".gserviceaccount.com")
  instance        = google_sql_database_instance.db.name
  type            = "CLOUD_IAM_SERVICE_ACCOUNT"
  deletion_policy = "ABANDON"
}

resource "google_project_iam_member" "app_sql" {
  for_each = toset(["roles/cloudsql.client", "roles/cloudsql.instanceUser"])
  project  = var.project
  role     = each.value
  member   = local.app_member
}

# ---- Secret Manager ----

# The app's configuration secret, read in the cluster through the Secret
# Manager add-on and secret synchronization as the app GSA.
resource "google_secret_manager_secret" "app_config" {
  secret_id = "ref-app-config"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "app_config" {
  secret      = google_secret_manager_secret.app_config.id
  secret_data = "ref-config-v1"
}

resource "google_secret_manager_secret_iam_member" "app_config" {
  secret_id = google_secret_manager_secret.app_config.id
  role      = "roles/secretmanager.secretAccessor"
  member    = local.app_member
}
