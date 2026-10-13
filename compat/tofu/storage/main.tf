# Cloud Storage: buckets, their IAM policy, objects, Pub/Sub
# notifications, HMAC keys, legacy ACLs and managed folders.

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

# Legacy ACLs need uniform bucket-level access off. The whole-list
# resources (bucket, default object and object ACL) and the single-entry
# access controls each get their own bucket or object, as each kind reads
# back every entry of the list it manages.

resource "google_storage_bucket" "d" {
  name                        = "${var.project}-tf-d"
  location                    = "US"
  uniform_bucket_level_access = false
  force_destroy               = true
}

resource "google_storage_bucket_acl" "d" {
  bucket      = google_storage_bucket.d.name
  role_entity = ["READER:group-readers@example.com"]
}

resource "google_storage_default_object_acl" "d" {
  bucket      = google_storage_bucket.d.name
  role_entity = ["READER:group-readers@example.com", "OWNER:group-admins@example.com"]
}

resource "google_storage_bucket_object" "d1" {
  bucket  = google_storage_bucket.d.name
  name    = "acl.txt"
  content = "object ACL\n"
}

resource "google_storage_object_acl" "d1" {
  bucket      = google_storage_bucket.d.name
  object      = google_storage_bucket_object.d1.output_name
  role_entity = ["READER:group-readers@example.com"]
}

resource "google_storage_bucket_object" "d2" {
  bucket  = google_storage_bucket.d.name
  name    = "access-control.txt"
  content = "object access control\n"
}

resource "google_storage_object_access_control" "d2" {
  bucket = google_storage_bucket.d.name
  object = google_storage_bucket_object.d2.output_name
  entity = "allAuthenticatedUsers"
  role   = "READER"
}

resource "google_storage_bucket" "e" {
  name                        = "${var.project}-tf-e"
  location                    = "US"
  uniform_bucket_level_access = false
  force_destroy               = true
}

resource "google_storage_bucket_access_control" "e" {
  bucket = google_storage_bucket.e.name
  entity = "group-writers@example.com"
  role   = "WRITER"
}

resource "google_storage_default_object_access_control" "e" {
  bucket = google_storage_bucket.e.name
  entity = "domain-example.com"
  role   = "READER"
}

# Managed folders need uniform bucket-level access on.

resource "google_storage_managed_folder" "shared" {
  bucket = google_storage_bucket.a.name
  name   = "shared/"
}

resource "google_storage_managed_folder_iam_member" "shared" {
  bucket         = google_storage_managed_folder.shared.bucket
  managed_folder = google_storage_managed_folder.shared.name
  role           = "roles/storage.objectViewer"
  member         = "serviceAccount:${google_service_account.s.email}"
}

resource "google_storage_managed_folder_iam_binding" "shared" {
  bucket         = google_storage_managed_folder.shared.bucket
  managed_folder = google_storage_managed_folder.shared.name
  role           = "roles/storage.objectCreator"
  members        = ["serviceAccount:${google_service_account.s.email}"]
}

resource "google_storage_managed_folder" "team" {
  bucket        = google_storage_bucket.a.name
  name          = "team/docs/"
  force_destroy = true
}

data "google_iam_policy" "team" {
  binding {
    role    = "roles/storage.objectAdmin"
    members = ["serviceAccount:${google_service_account.s.email}"]
  }
}

resource "google_storage_managed_folder_iam_policy" "team" {
  bucket         = google_storage_managed_folder.team.bucket
  managed_folder = google_storage_managed_folder.team.name
  policy_data    = data.google_iam_policy.team.policy_data
}
