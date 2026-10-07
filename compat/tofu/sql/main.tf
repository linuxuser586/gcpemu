# Cloud SQL: a PostgreSQL instance with flags, backups and authorized
# networks, a database, built-in and IAM users and a client certificate.

resource "google_sql_database_instance" "pg" {
  name                = "tf-sql-pg"
  database_version    = "POSTGRES_16"
  region              = var.region
  deletion_protection = false

  settings {
    tier              = "db-custom-1-3840"
    availability_type = "ZONAL"
    disk_size         = 10
    disk_type         = "PD_SSD"
    user_labels       = { env = "acceptance" }
    ip_configuration {
      ipv4_enabled = true
      authorized_networks {
        name  = "anywhere"
        value = "0.0.0.0/0"
      }
    }
    database_flags {
      name  = "cloudsql.iam_authentication"
      value = "on"
    }
    database_flags {
      name  = "max_connections"
      value = "50"
    }
    backup_configuration {
      enabled    = true
      start_time = "03:00"
    }
    maintenance_window {
      day  = 7
      hour = 3
    }
  }
}

resource "google_sql_database" "app" {
  name     = "app"
  instance = google_sql_database_instance.pg.name
}

resource "google_sql_user" "builtin" {
  name     = "alice"
  instance = google_sql_database_instance.pg.name
  password = "alice-password-0123"
}

resource "google_service_account" "db" {
  account_id = "tf-sql"
}

resource "google_sql_user" "iam" {
  name     = trimsuffix(google_service_account.db.email, ".gserviceaccount.com")
  instance = google_sql_database_instance.pg.name
  type     = "CLOUD_IAM_SERVICE_ACCOUNT"
}

resource "google_sql_ssl_cert" "client" {
  common_name = "tf-sql-client"
  instance    = google_sql_database_instance.pg.name
}
