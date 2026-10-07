# IAM and Resource Manager: service accounts, their keys and IAM policy,
# project IAM, custom roles and Workload Identity Federation.

resource "google_service_account" "a" {
  account_id   = "tf-iam-a"
  display_name = "acceptance a"
  description  = "service account under test"
}

resource "google_service_account" "b" {
  account_id   = "tf-iam-b"
  display_name = "acceptance b"
}

resource "google_service_account" "c" {
  account_id   = "tf-iam-c"
  display_name = "acceptance c"
}

resource "google_service_account_key" "a" {
  service_account_id = google_service_account.a.name
}

resource "google_service_account_iam_member" "a" {
  service_account_id = google_service_account.a.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${google_service_account.b.email}"
}

resource "google_service_account_iam_binding" "b" {
  service_account_id = google_service_account.b.name
  role               = "roles/iam.serviceAccountUser"
  members            = ["serviceAccount:${google_service_account.a.email}"]
}

data "google_iam_policy" "c" {
  binding {
    role    = "roles/iam.workloadIdentityUser"
    members = ["serviceAccount:${var.project}.svc.id.goog[ns/ksa]"]
  }
}

resource "google_service_account_iam_policy" "c" {
  service_account_id = google_service_account.c.name
  policy_data        = data.google_iam_policy.c.policy_data
}

resource "google_project_iam_custom_role" "r" {
  role_id     = "tfAcceptance"
  title       = "Acceptance"
  description = "custom role under test"
  permissions = ["storage.buckets.get", "storage.objects.list"]
}

resource "google_project_iam_member" "m" {
  project = var.project
  role    = "roles/viewer"
  member  = "serviceAccount:${google_service_account.a.email}"
}

resource "google_project_iam_binding" "b" {
  project = var.project
  role    = google_project_iam_custom_role.r.id
  members = ["serviceAccount:${google_service_account.b.email}"]
}

resource "google_iam_workload_identity_pool" "p" {
  workload_identity_pool_id = "tf-pool"
  display_name              = "acceptance"
  description               = "pool under test"
}

resource "google_iam_workload_identity_pool_provider" "gh" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.p.workload_identity_pool_id
  workload_identity_pool_provider_id = "github"
  display_name                       = "GitHub Actions"
  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
  }
  attribute_condition = "assertion.repository_owner == \"linuxuser586\""
  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}
