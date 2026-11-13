# Remote state in GCS (a few KB: well inside the Cloud Storage free tier).
# Create the bucket out of band before the first `terraform init` (see
# infra/README.md, "Bootstrap the lab").
terraform {
  backend "gcs" {
    bucket = "flowforge-lab-tfstate"
    prefix = "envs/lab"
  }
}
