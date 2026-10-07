output "project" {
  value = var.project
}

output "zone" {
  value = var.zone
}

output "app_host" {
  value = local.app_host
}

output "lb_ip" {
  value = google_compute_global_address.lb.address
}

output "url_map" {
  value = google_compute_url_map.web.name
}

output "cluster" {
  value = google_container_cluster.gke.id
}

output "image_repo" {
  description = "Docker repository for the app image"
  value       = "${google_artifact_registry_repository.app.location}-docker.pkg.dev/${var.project}/${google_artifact_registry_repository.app.repository_id}"
}

output "uploads_bucket" {
  value = google_storage_bucket.uploads.name
}

output "static_bucket" {
  value = google_storage_bucket.static.name
}

output "topic" {
  value = google_pubsub_topic.events.name
}

output "app_gsa" {
  value = google_service_account.app.email
}

output "push_sa" {
  value = google_service_account.push.email
}

output "push_audience" {
  value = google_pubsub_subscription.push.push_config[0].oidc_token[0].audience
}

output "sql_connection_name" {
  value = google_sql_database_instance.db.connection_name
}

output "sql_private_ip" {
  value = google_sql_database_instance.db.private_ip_address
}

output "sql_iam_user" {
  value = google_sql_user.app.name
}

output "gateway_sni" {
  value = local.gateway_name
}

output "gateway_cert_pem" {
  value = tls_locally_signed_cert.gateway.cert_pem
}

output "gateway_key_pem" {
  value     = tls_private_key.gateway.private_key_pem
  sensitive = true
}

output "mesh_ca_pem" {
  value = tls_self_signed_cert.mesh_ca.cert_pem
}

output "lb_client_ca_pem" {
  value = tls_self_signed_cert.client_ca.cert_pem
}
