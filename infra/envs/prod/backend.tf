# Remote state in GCS; the backend takes a lock object per operation. The
# bucket is created out of band (versioning on, uniform access, no public
# access) before the first `terraform init`.
terraform {
  backend "gcs" {
    bucket = "flowforge-prod-tfstate"
    prefix = "envs/prod"
  }
}
