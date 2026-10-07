# Cloud Storage: buckets, their IAM policy, objects, Pub/Sub
# notifications and HMAC keys.

resource "google_storage_bucket" "a" {
  name                        = "${var.project}-tf-a"
  location                    = "US"
  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  force_destroy               = true
  labels                      = { env = "acceptance" }

  versioning {
    enabled = true
  }
  lifecycle_rule {
    condition {
      age = 30
    }
    action {
      type = "Delete"
    }
  }
  cors {
    origin          = ["https://example.test"]
    method          = ["GET"]
    response_header = ["Content-Type"]
    max_age_seconds = 600
  }
  website {
    main_page_suffix = "index.html"
    not_found_page   = "404.html"
  }
}

resource "google_storage_bucket" "b" {
  name                        = "${var.project}-tf-b"
  location                    = var.region
  uniform_bucket_level_access = true
  force_destroy               = true
}

resource "google_storage_bucket" "c" {
  name                        = "${var.project}-tf-c"
  location                    = "EU"
  uniform_bucket_level_access = true
  force_destroy               = true
}

resource "google_storage_bucket_object" "text" {
  bucket       = google_storage_bucket.a.name
  name         = "dir/hello.txt"
  content      = "hello from tofu\n"
  content_type = "text/plain"
  metadata     = { k = "v" }
}

resource "google_storage_bucket_object" "file" {
  bucket = google_storage_bucket.a.name
  name   = "main.tf"
  source = "${path.module}/main.tf"
}

resource "google_service_account" "s" {
  account_id = "tf-storage"
}

resource "google_storage_bucket_iam_member" "a" {
  bucket = google_storage_bucket.a.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.s.email}"
}

resource "google_storage_bucket_iam_binding" "b" {
  bucket  = google_storage_bucket.b.name
  role    = "roles/storage.objectAdmin"
  members = ["serviceAccount:${google_service_account.s.email}"]
}

data "google_iam_policy" "c" {
  binding {
    role    = "roles/storage.admin"
    members = ["serviceAccount:${google_service_account.s.email}"]
  }
}

resource "google_storage_bucket_iam_policy" "c" {
  bucket      = google_storage_bucket.c.name
  policy_data = data.google_iam_policy.c.policy_data
}

resource "google_storage_hmac_key" "k" {
  service_account_email = google_service_account.s.email
}

resource "google_pubsub_topic" "n" {
  name = "tf-storage-notify"
}

data "google_storage_project_service_account" "gcs" {}

resource "google_pubsub_topic_iam_member" "gcs" {
  topic  = google_pubsub_topic.n.id
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:${data.google_storage_project_service_account.gcs.email_address}"
}

resource "google_storage_notification" "n" {
  bucket             = google_storage_bucket.b.name
  topic              = google_pubsub_topic.n.id
  payload_format     = "JSON_API_V1"
  event_types        = ["OBJECT_FINALIZE", "OBJECT_DELETE"]
  object_name_prefix = "in/"
  custom_attributes  = { origin = "tofu" }
  depends_on         = [google_pubsub_topic_iam_member.gcs]
}
