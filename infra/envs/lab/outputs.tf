output "api_url" {
  description = "Public API (the gateway's run.app URL; no load balancer in the lab). FF_API_DOMAIN for the deploy smoke test is its host."
  value       = module.svc["gateway"].uri
}

output "service_urls" {
  description = "Cloud Run service URLs."
  value       = { for s, m in module.svc : s => m.uri }
}

output "vm" {
  description = "The lab VM: name, zone, internal IP (Postgres, Redis, proxy)."
  value = {
    name        = google_compute_instance.vm.name
    zone        = local.zone
    internal_ip = google_compute_address.vm.address
    proxy       = local.proxy_url
    ssh         = "gcloud compute ssh ${google_compute_instance.vm.name} --zone ${local.zone} --tunnel-through-iap --project ${var.project_id}"
  }
}

output "artifact_registry" {
  description = "Docker repository for images."
  value       = "${local.registry}/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

output "ci_service_account" {
  description = "sa-ci email for GitHub Actions (vars.GCP_DEPLOY_SA of the lab environment)."
  value       = google_service_account.ci.email
}

output "workload_identity_provider" {
  description = "Full provider name for google-github-actions/auth (vars.GCP_WIF_PROVIDER)."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "secrets" {
  description = "Secret Manager secrets (each needs one version; ff-db-password and ff-redis-auth are written by Terraform)."
  value       = local.secret_names
}

output "topics" {
  description = "Topic => subscriptions."
  value       = { for t, m in module.topic : m.topic_name => m.subscriptions }
}

output "power" {
  description = "Names used by infra/scripts/gcp.sh (make gcp-lab-pause / gcp-lab-resume / gcp-lab-cost)."
  value = {
    project        = var.project_id
    region         = var.region
    gateway        = module.svc["gateway"].name
    services       = join(" ", [for s, m in module.svc : m.name])
    warm_services  = ""
    scheduler_jobs = google_cloud_scheduler_job.tick.name
    sql_instance   = ""
    vm             = google_compute_instance.vm.name
    vm_zone        = local.zone
  }
}
