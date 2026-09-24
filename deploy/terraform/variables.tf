variable "project" {
  description = "GCP project id"
  type        = string
}

variable "region" {
  type    = string
  default = "asia-south1"
}

variable "zone" {
  type    = string
  default = "asia-south1-a"
}

variable "domain" {
  description = "Hostname for the web app, webhooks and tracked links (point its A record at the static IP)"
  type        = string
}

variable "machine_type" {
  description = "e2-small is enough for one operator; Postgres runs on the same VM"
  type        = string
  default     = "e2-small"
}

variable "disk_gb" {
  type    = number
  default = 20
}

variable "backup_retention_days" {
  type    = number
  default = 30
}
