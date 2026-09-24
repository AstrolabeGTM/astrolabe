output "ip" {
  description = "Point the domain's A record here"
  value       = google_compute_address.ip.address
}

output "backups_bucket" {
  value = google_storage_bucket.backups.name
}

output "next_steps" {
  value = <<-EOT
    1. DNS: A record ${var.domain} -> ${google_compute_address.ip.address}
    2. Secrets: gcloud secrets versions add ${google_secret_manager_secret.env.secret_id} --data-file=astrolabe.env
       (ASTROLABE_PASSWORD, ASTROLABE_PUBLIC_URL=https://${var.domain}, TZ=Asia/Kolkata, API keys, webhook secrets;
        the startup script sets ASTROLABE_DATABASE_URL for the local Postgres)
    3. Deploy: ../deploy.sh ${var.project} ${var.zone}
  EOT
}
