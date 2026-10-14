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

# AWS and SAML providers are managed only: the emulator does not exchange
# their credentials.
resource "google_iam_workload_identity_pool_provider" "aws" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.p.workload_identity_pool_id
  workload_identity_pool_provider_id = "aws-acct"
  aws {
    account_id = "999999999999"
  }
}

resource "google_iam_workload_identity_pool_provider" "saml" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.p.workload_identity_pool_id
  workload_identity_pool_provider_id = "saml"
  attribute_mapping = {
    "google.subject" = "assertion.subject"
  }
  saml {
    idp_metadata_xml = <<-EOT
      <md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.com">
        <md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
          <md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.com/sso"/>
        </md:IDPSSODescriptor>
      </md:EntityDescriptor>
    EOT
  }
}

# Modules read the project number for service agents and Workload Identity
# principals. IAM resolves the default compute account only from the number
# it uses itself, so the binding fails if the two disagree.
data "google_project" "this" {}

resource "google_service_account_iam_member" "compute" {
  service_account_id = "projects/${var.project}/serviceAccounts/${data.google_project.this.number}-compute@developer.gserviceaccount.com"
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.a.email}"
}

# Two at once go through one services:batchEnable; destroy disables them.
resource "google_project_service" "kms" {
  service = "cloudkms.googleapis.com"
}

resource "google_project_service" "logging" {
  service = "logging.googleapis.com"
}

# IAM v2: a deny policy guarding Secret versions against destruction, as
# polyappic's secret store guards its seal and backup keys (#184). The
# acceptance runs at IAM enforcement level audit, so other modules' destroys
# still go through.
resource "google_iam_deny_policy" "no_destroy" {
  parent       = urlencode("cloudresourcemanager.googleapis.com/projects/${var.project}")
  name         = "tf-no-destroy"
  display_name = "No Secret version destruction"
  rules {
    description = "only breakglass destroys Secret versions"
    deny_rule {
      denied_principals    = ["principalSet://goog/public:all"]
      exception_principals = ["principal://goog/subject/breakglass@example.com"]
      denied_permissions   = ["secretmanager.googleapis.com/versions.destroy"]
    }
  }
}
