# Pub/Sub: schemas, topics, pull and push subscriptions with dead
# lettering, and topic and subscription IAM policy.

resource "google_pubsub_schema" "s" {
  name       = "tf-schema"
  type       = "AVRO"
  definition = jsonencode({
    type   = "record"
    name   = "Event"
    fields = [{ name = "id", type = "string" }]
  })
}

resource "google_pubsub_topic" "a" {
  name                       = "tf-topic-a"
  labels                     = { env = "acceptance" }
  message_retention_duration = "86400s"
  schema_settings {
    schema   = google_pubsub_schema.s.id
    encoding = "JSON"
  }
}

resource "google_pubsub_topic" "b" {
  name = "tf-topic-b"
}

resource "google_pubsub_topic" "dead" {
  name = "tf-topic-dead"
}

resource "google_pubsub_subscription" "pull" {
  name                       = "tf-pull"
  topic                      = google_pubsub_topic.a.id
  ack_deadline_seconds       = 30
  message_retention_duration = "1200s"
  retain_acked_messages      = true
  enable_message_ordering    = true
  filter                     = "attributes.kind = \"x\""
  labels                     = { env = "acceptance" }
  expiration_policy {
    ttl = "300000.5s"
  }
  retry_policy {
    minimum_backoff = "5s"
    maximum_backoff = "60s"
  }
  dead_letter_policy {
    dead_letter_topic     = google_pubsub_topic.dead.id
    max_delivery_attempts = 5
  }
}

resource "google_service_account" "push" {
  account_id = "tf-push"
}

resource "google_pubsub_subscription" "push" {
  name  = "tf-push"
  topic = google_pubsub_topic.b.id
  push_config {
    push_endpoint = "https://push.example.test/hook"
    attributes    = { x-goog-version = "v1" }
    oidc_token {
      service_account_email = google_service_account.push.email
      audience              = "https://push.example.test/hook"
    }
  }
}

resource "google_pubsub_topic_iam_member" "a" {
  topic  = google_pubsub_topic.a.id
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:${google_service_account.push.email}"
}

resource "google_pubsub_topic_iam_binding" "b" {
  topic   = google_pubsub_topic.b.id
  role    = "roles/pubsub.viewer"
  members = ["serviceAccount:${google_service_account.push.email}"]
}

data "google_iam_policy" "subscriber" {
  binding {
    role    = "roles/pubsub.subscriber"
    members = ["serviceAccount:${google_service_account.push.email}"]
  }
}

resource "google_pubsub_topic_iam_policy" "dead" {
  topic       = google_pubsub_topic.dead.id
  policy_data = data.google_iam_policy.subscriber.policy_data
}

resource "google_pubsub_subscription_iam_member" "pull" {
  subscription = google_pubsub_subscription.pull.id
  role         = "roles/pubsub.subscriber"
  member       = "serviceAccount:${google_service_account.push.email}"
}

resource "google_pubsub_subscription_iam_binding" "pull" {
  subscription = google_pubsub_subscription.pull.id
  role         = "roles/pubsub.viewer"
  members      = ["serviceAccount:${google_service_account.push.email}"]
}

resource "google_pubsub_subscription_iam_policy" "push" {
  subscription = google_pubsub_subscription.push.id
  policy_data  = data.google_iam_policy.subscriber.policy_data
}
