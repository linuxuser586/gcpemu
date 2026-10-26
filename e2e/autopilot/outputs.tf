output "cluster" {
  value = google_container_cluster.ap.id
}

output "master_version" {
  value = google_container_cluster.ap.master_version
}
