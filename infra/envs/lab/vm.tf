# The lab VM (G-OPS-19): ONE e2-micro Container-Optimized OS instance in a
# free-tier region running, as containers managed by systemd units that
# cloud-init writes on every boot:
#   ff-postgres       Postgres 16 (pg_trgm, citext), data on the data disk
#   ff-redis          Redis 7, optional (redis_enabled)
#   ff-egress-proxy   the Flowforge egress proxy, HTTP + CONNECT on proxy_port
# SOCKS5 (1080) and DNS (53) are reserved for the v2 proxy (G-OPS-15) and
# not opened. Only the Cloud Run Direct VPC egress subnets reach these ports
# (VPC firewall + the host's iptables); SSH only through IAP.

data "google_compute_image" "cos" {
  project = "cos-cloud"
  family  = var.vm_image_family
}

resource "google_service_account" "vm" {
  project      = var.project_id
  account_id   = "sa-lab-vm"
  display_name = "Flowforge lab VM (Postgres, Redis, egress proxy)"
}

# COS logging agent.
resource "google_project_iam_member" "vm_logging" {
  count   = var.vm_logging_enabled ? 1 : 0
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = google_service_account.vm.member
}

resource "google_artifact_registry_repository_iam_member" "vm_reader" {
  project    = var.project_id
  location   = var.region
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.vm.member
}

# The database password (Postgres superuser + the proxy's DSN), the signing
# keys (the proxy calls scope) and the Redis password.
resource "google_secret_manager_secret_iam_member" "vm" {
  for_each  = toset(concat([local.db_password], values(local.vm_proxy_secrets), var.redis_enabled ? ["ff-redis-auth"] : []))
  project   = var.project_id
  secret_id = google_secret_manager_secret.this[each.value].id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.vm.member
}

resource "google_cloud_run_v2_service_iam_member" "vm_scope" {
  project  = var.project_id
  location = var.region
  name     = module.svc["scope"].name
  role     = "roles/run.invoker"
  member   = google_service_account.vm.member
}

resource "google_compute_address" "vm" {
  project      = var.project_id
  name         = "${local.name}-vm"
  region       = var.region
  address_type = "INTERNAL" # internal addresses are not billed; the external one is ephemeral
  subnetwork   = module.network.egress_subnet_id
}

resource "google_compute_disk" "data" {
  project = var.project_id
  name    = "${local.name}-data"
  zone    = local.zone
  type    = var.vm_disk_type
  size    = var.vm_data_disk_gb
  labels  = local.labels

  depends_on = [terraform_data.guardrails]
}

locals {
  registry = "${var.region}-docker.pkg.dev"

  vm_proxy_secrets = { for k, v in local.signing_key_env : k => v.secret }

  vm_proxy_env = merge(local.env_common, local.env_signing, local.addr["scope"], {
    PORT      = tostring(var.proxy_port)
    FF_DB_DSN = "host=127.0.0.1 port=5432 dbname=flowforge user=flowforge sslmode=disable"
  })

  vm_tpl = {
    project         = var.project_id
    services_cidr   = var.services_cidr
    tools_cidr      = var.tools_cidr
    proxy_port      = var.proxy_port
    redis_enabled   = var.redis_enabled
    db_secret       = local.db_password
    redis_secret    = "ff-redis-auth"
    proxy_secrets   = local.vm_proxy_secrets
    pg_image        = var.vm_postgres_image
    redis_image     = var.vm_redis_image
    max_connections = var.db_max_connections
    registry        = local.registry
  }

  unit = {
    "ff-data" = <<-EOT
      [Unit]
      Description=Flowforge data disk
      After=local-fs.target
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/bin/bash /etc/ff/ff-data.sh
    EOT
    "ff-firewall" = <<-EOT
      [Unit]
      Description=Flowforge host firewall openings
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/bin/bash /etc/ff/ff-firewall.sh
    EOT
    "ff-secrets" = <<-EOT
      [Unit]
      Description=Flowforge secrets from Secret Manager
      Wants=network-online.target
      After=network-online.target ff-data.service
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/bin/bash /etc/ff/ff-secrets.sh
      Restart=on-failure
      RestartSec=30
    EOT
    "ff-postgres" = <<-EOT
      [Unit]
      Description=Flowforge Postgres 16
      Requires=docker.service ff-data.service ff-secrets.service
      After=docker.service ff-data.service ff-secrets.service ff-firewall.service
      [Service]
      ExecStartPre=/bin/bash /etc/ff/ff-pg-tls.sh
      ExecStart=/bin/bash /etc/ff/run-postgres.sh
      ExecStop=/usr/bin/docker stop -t 60 ff-postgres
      TimeoutStopSec=90
      Restart=always
      RestartSec=10
    EOT
    "ff-redis" = <<-EOT
      [Unit]
      Description=Flowforge Redis 7
      Requires=docker.service ff-data.service ff-secrets.service
      After=docker.service ff-data.service ff-secrets.service ff-firewall.service
      [Service]
      ExecStart=/bin/bash /etc/ff/run-redis.sh
      ExecStop=/usr/bin/docker stop -t 20 ff-redis
      Restart=always
      RestartSec=10
    EOT
    "ff-egress-proxy" = <<-EOT
      [Unit]
      Description=Flowforge egress proxy
      Requires=docker.service ff-secrets.service
      Wants=ff-postgres.service
      After=docker.service ff-secrets.service ff-postgres.service ff-firewall.service
      [Service]
      ExecStart=/bin/bash /etc/ff/run-egress-proxy.sh
      ExecStop=/usr/bin/docker stop -t 20 ff-egress-proxy
      Restart=always
      RestartSec=15
    EOT
    "ff-image-watch" = <<-EOT
      [Unit]
      Description=Restart the egress proxy when its image metadata changes
      Wants=network-online.target
      After=network-online.target ff-egress-proxy.service
      [Service]
      ExecStart=/bin/bash /etc/ff/ff-image-watch.sh
      Restart=always
      RestartSec=30
    EOT
  }

  started_units = concat(
    ["ff-data", "ff-firewall", "ff-secrets", "ff-postgres"],
    var.redis_enabled ? ["ff-redis"] : [],
    ["ff-egress-proxy", "ff-image-watch"],
  )

  vm_files = concat(
    [
      { path = "/etc/ff/ff-data.sh", content = file("${path.module}/vm/ff-data.sh") },
      { path = "/etc/ff/ff-image-watch.sh", content = file("${path.module}/vm/ff-image-watch.sh") },
      { path = "/etc/ff/ff-firewall.sh", content = templatefile("${path.module}/vm/ff-firewall.sh.tftpl", local.vm_tpl) },
      { path = "/etc/ff/ff-secrets.sh", content = templatefile("${path.module}/vm/ff-secrets.sh.tftpl", local.vm_tpl) },
      { path = "/etc/ff/ff-pg-tls.sh", content = templatefile("${path.module}/vm/ff-pg-tls.sh.tftpl", local.vm_tpl) },
      { path = "/etc/ff/run-postgres.sh", content = templatefile("${path.module}/vm/run-postgres.sh.tftpl", local.vm_tpl) },
      { path = "/etc/ff/run-redis.sh", content = templatefile("${path.module}/vm/run-redis.sh.tftpl", local.vm_tpl) },
      { path = "/etc/ff/run-egress-proxy.sh", content = templatefile("${path.module}/vm/run-egress-proxy.sh.tftpl", local.vm_tpl) },
      # Same extensions as the local compose stack (deploy/initdb).
      { path = "/etc/ff/initdb/01-extensions.sql", content = file("${path.module}/../../../deploy/initdb/01-extensions.sql") },
      { path = "/etc/ff/egress-proxy.env", content = join("", [for k in sort(keys(local.vm_proxy_env)) : "${k}=${local.vm_proxy_env[k]}\n"]) },
    ],
    [for n, u in local.unit : { path = "/etc/systemd/system/${n}.service", content = u } if contains(local.started_units, n)],
  )

  # COS processes user-data with cloud-init on every boot (/etc is not
  # persistent), so the units are rewritten and started each time.
  cloud_init = "#cloud-config\n${yamlencode({
    write_files = [for f in local.vm_files : {
      path        = f.path
      permissions = "0644"
      owner       = "root"
      encoding    = "b64"
      content     = base64encode(f.content)
    }]
    runcmd = [
      ["systemctl", "daemon-reload"],
      concat(["systemctl", "start"], [for u in local.started_units : "${u}.service"]),
    ]
  })}"
}

resource "google_compute_instance" "vm" {
  project      = var.project_id
  name         = "${local.name}-vm"
  zone         = local.zone
  machine_type = var.vm_machine_type
  description  = "Flowforge lab: Postgres, Redis (optional), egress proxy"
  tags         = ["ff-lab-vm", module.network.egress_proxy_tag]
  labels       = local.labels

  allow_stopping_for_update = true
  deletion_protection       = var.deletion_protection

  boot_disk {
    auto_delete = true
    initialize_params {
      image  = data.google_compute_image.cos.self_link
      size   = var.vm_boot_disk_gb
      type   = var.vm_disk_type
      labels = local.labels
    }
  }

  attached_disk {
    source      = google_compute_disk.data.id
    device_name = "ff-data"
  }

  network_interface {
    subnetwork = module.network.egress_subnet_id
    network_ip = google_compute_address.vm.address

    # Ephemeral external IPv4: the proxy's path to the internet (no Cloud
    # NAT). Billed while the VM runs; released when it is stopped.
    access_config {}
  }

  service_account {
    email  = google_service_account.vm.email
    scopes = ["cloud-platform"]
  }

  # The free tier covers a standard (non-Spot, non-preemptible) e2-micro.
  scheduling {
    preemptible         = false
    provisioning_model  = "STANDARD"
    automatic_restart   = true
    on_host_maintenance = "MIGRATE"
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  metadata = {
    user-data                 = local.cloud_init
    google-logging-enabled    = tostring(var.vm_logging_enabled)
    google-monitoring-enabled = "false"
    enable-oslogin            = "TRUE"
    block-project-ssh-keys    = "TRUE"
    # Rolled forward by .github/workflows/deploy.yml (lab); ff-image-watch
    # restarts the proxy when it changes.
    ff-egress-proxy-image = var.images["egress-proxy"]
  }

  lifecycle {
    ignore_changes = [
      metadata["ff-egress-proxy-image"],
      # A new cos-stable image would otherwise replace the VM on every apply.
      boot_disk[0].initialize_params[0].image,
    ]
  }

  depends_on = [
    terraform_data.guardrails,
    google_secret_manager_secret_iam_member.vm,
    google_secret_manager_secret_version.db,
    google_artifact_registry_repository_iam_member.vm_reader,
  ]
}

# ---------------------------------------------------------------- firewall

resource "google_compute_firewall" "vm_db" {
  project       = var.project_id
  name          = "${local.name}-vm-allow-db"
  network       = module.network.network_id
  direction     = "INGRESS"
  priority      = 1000
  target_tags   = ["ff-lab-vm"]
  source_ranges = [var.services_cidr]
  description   = "Postgres (and Redis) from the Cloud Run services subnet only"

  allow {
    protocol = "tcp"
    ports    = var.redis_enabled ? ["5432", "6379"] : ["5432"]
  }
}

resource "google_compute_firewall" "vm_proxy" {
  project       = var.project_id
  name          = "${local.name}-vm-allow-proxy"
  network       = module.network.network_id
  direction     = "INGRESS"
  priority      = 1000
  target_tags   = ["ff-lab-vm"]
  source_ranges = [var.services_cidr, var.tools_cidr]
  description   = "Egress proxy (HTTP/CONNECT) from the services and tool-runner subnets"

  allow {
    protocol = "tcp"
    ports    = [tostring(var.proxy_port)]
  }
}

# IAP TCP forwarding range: `gcloud compute ssh --tunnel-through-iap`.
resource "google_compute_firewall" "vm_iap_ssh" {
  project       = var.project_id
  name          = "${local.name}-vm-allow-iap-ssh"
  network       = module.network.network_id
  direction     = "INGRESS"
  priority      = 1000
  target_tags   = ["ff-lab-vm"]
  source_ranges = ["35.235.240.0/20"]
  description   = "SSH through Identity-Aware Proxy only"

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# The tool-runner's default-deny egress (network module) gets one exception:
# the proxy on the VM.
resource "google_compute_firewall" "tools_to_proxy" {
  project            = var.project_id
  name               = "${local.name}-tools-allow-proxy"
  network            = module.network.network_id
  direction          = "EGRESS"
  priority           = 1000
  target_tags        = [module.network.tool_runner_tag]
  destination_ranges = ["${google_compute_address.vm.address}/32"]

  allow {
    protocol = "tcp"
    ports    = [tostring(var.proxy_port)]
  }
}
