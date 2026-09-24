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
       (ASTROLABE_MODE=live, ASTROLABE_PASSWORD, ASTROLABE_PUBLIC_URL=https://${var.domain}, TZ, API keys, webhook secrets)
    3. Deploy your workspace's products: ../deploy.sh ${var.project} ${var.zone} <workspace dir>
    4. Connect mailboxes: gcloud compute ssh astrolabe --tunnel-through-iap, then
       cd /opt/astrolabe && sudo docker compose exec astrolabe astrolabe mailbox add you@yourdomain.com
  EOT
}
