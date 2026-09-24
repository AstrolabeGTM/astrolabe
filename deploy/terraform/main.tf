# One VM: astrolabe + Postgres + Caddy (automatic HTTPS). Secrets live in
# Secret Manager as one env file; daily database dumps go to a versioned
# bucket. Cloud SQL can replace local Postgres later by changing
# ASTROLABE_DATABASE_URL in the secret.

resource "google_project_service" "apis" {
  for_each           = toset(["compute.googleapis.com", "secretmanager.googleapis.com", "iap.googleapis.com"])
  service            = each.value
  disable_on_destroy = false
}

resource "google_service_account" "vm" {
  account_id   = "astrolabe-vm"
  display_name = "Astrolabe VM"
}

resource "google_project_iam_member" "vm_logs" {
  project = var.project
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.vm.email}"
}

# The whole environment (ASTROLABE_* variables) as one secret. Add a version
# with: gcloud secrets versions add astrolabe-env --data-file=astrolabe.env
resource "google_secret_manager_secret" "env" {
  secret_id = "astrolabe-env"
  replication {
    auto {}
  }
  depends_on = [google_project_service.apis]
}

resource "google_secret_manager_secret_iam_member" "vm_reads_env" {
  secret_id = google_secret_manager_secret.env.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.vm.email}"
}

resource "google_storage_bucket" "backups" {
  name                        = "${var.project}-astrolabe-backups"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  versioning {
    enabled = true
  }
  lifecycle_rule {
    condition {
      age = var.backup_retention_days
    }
    action {
      type = "Delete"
    }
  }
}

resource "google_storage_bucket_iam_member" "vm_writes_backups" {
  bucket = google_storage_bucket.backups.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.vm.email}"
}

resource "google_compute_address" "ip" {
  name       = "astrolabe"
  depends_on = [google_project_service.apis]
}

resource "google_compute_firewall" "web" {
  name          = "astrolabe-web"
  network       = "default"
  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["astrolabe"]
  allow {
    protocol = "tcp"
    ports    = ["80", "443"]
  }
}

# SSH only through Identity-Aware Proxy (gcloud compute ssh --tunnel-through-iap).
resource "google_compute_firewall" "iap_ssh" {
  name          = "astrolabe-iap-ssh"
  network       = "default"
  source_ranges = ["35.235.240.0/20"]
  target_tags   = ["astrolabe"]
  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

resource "google_compute_instance" "vm" {
  name         = "astrolabe"
  machine_type = var.machine_type
  tags         = ["astrolabe"]

  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
      size  = var.disk_gb
      type  = "pd-balanced"
    }
  }

  network_interface {
    network = "default"
    access_config {
      nat_ip = google_compute_address.ip.address
    }
  }

  service_account {
    email  = google_service_account.vm.email
    scopes = ["cloud-platform"]
  }

  shielded_instance_config {
    enable_secure_boot = true
  }

  metadata = {
    enable-oslogin = "TRUE"
  }

  metadata_startup_script = templatefile("${path.module}/startup.sh.tftpl", {
    domain = var.domain
    bucket = google_storage_bucket.backups.name
    secret = google_secret_manager_secret.env.secret_id
  })

  allow_stopping_for_update = true
  depends_on                = [google_project_service.apis]
}
