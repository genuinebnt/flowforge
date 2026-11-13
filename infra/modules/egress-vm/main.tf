# The v2 egress proxy on one Container-Optimized OS VM (docs/GAPS.md
# G-OPS-15, G-SVC-14; V2_DESIGN §9). Cloud Run serves HTTP requests only,
# so the proxy's CONNECT tunnels to any port, SOCKS5 and DNS over UDP and
# TCP cannot run there. This is the egress-proxy half of the lab VM
# (infra/envs/lab/vm.tf: the same systemd units, Secret Manager fetch,
# metadata-driven image rollout and host firewall) without Postgres and
# Redis, for dev and prod:
#
#   - an internal static address in the egress subnet and no external IP:
#     traffic leaves through Cloud NAT's reserved IPs, which programs
#     allowlist (spec 17.1);
#   - VPC firewall: every proxy port from the tool-runner subnet only, the
#     HTTP port also from http_client_cidrs (the notifier's notify path);
#     the tool-runner's default-deny egress gets one exception, to this
#     address; SSH only through IAP;
#   - the container runs read-only, unprivileged, with every capability
#     dropped; the host's iptables repeats the source ranges.

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

data "google_compute_image" "cos" {
  project = "cos-cloud"
  family  = var.image_family
}

resource "google_service_account" "this" {
  project      = var.project_id
  account_id   = var.account_id
  display_name = "Flowforge egress proxy VM (${var.name})"
}

resource "google_project_iam_member" "logging" {
  count   = var.logging_enabled ? 1 : 0
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = google_service_account.this.member
}

resource "google_artifact_registry_repository_iam_member" "reader" {
  project    = var.project_id
  location   = var.region
  repository = var.repository
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.this.member
}

resource "google_secret_manager_secret_iam_member" "secrets" {
  for_each  = var.secret_ids
  project   = var.project_id
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.this.member
}

resource "google_compute_address" "this" {
  project      = var.project_id
  name         = "${var.name}-egress"
  region       = var.region
  address_type = "INTERNAL"
  subnetwork   = var.subnetwork_id
}

locals {
  socks_addr = "${google_compute_address.this.address}:${var.socks_port}"
  dns_addr   = "${google_compute_address.this.address}:${var.dns_port}"

  proxy_env = merge(var.env, {
    PORT                      = tostring(var.proxy_port)
    FF_EGRESS_SOCKS_LISTEN    = ":${var.socks_port}"
    FF_EGRESS_SOCKS_ADVERTISE = local.socks_addr
    FF_EGRESS_DNS_LISTEN      = ":${var.dns_port}"
    FF_EGRESS_DNS_ADVERTISE   = local.dns_addr
  })

  tpl = {
    project           = var.project_id
    registry          = var.registry
    proxy_port        = var.proxy_port
    socks_port        = var.socks_port
    dns_port          = var.dns_port
    tool_cidrs        = var.tool_cidrs
    http_client_cidrs = var.http_client_cidrs
    secrets           = var.secret_env
  }

  units = {
    "ff-firewall"     = <<-EOT
      [Unit]
      Description=Flowforge host firewall openings
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/bin/bash /etc/ff/ff-firewall.sh
    EOT
    "ff-secrets"      = <<-EOT
      [Unit]
      Description=Flowforge secrets from Secret Manager
      Wants=network-online.target
      After=network-online.target
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/bin/bash /etc/ff/ff-secrets.sh
      Restart=on-failure
      RestartSec=30
    EOT
    "ff-egress-proxy" = <<-EOT
      [Unit]
      Description=Flowforge egress proxy
      Requires=docker.service ff-secrets.service
      After=docker.service ff-secrets.service ff-firewall.service
      [Service]
      ExecStart=/bin/bash /etc/ff/run-egress-proxy.sh
      ExecStop=/usr/bin/docker stop -t 20 ff-egress-proxy
      Restart=always
      RestartSec=15
    EOT
    "ff-image-watch"  = <<-EOT
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

  files = concat(
    [
      { path = "/etc/ff/ff-firewall.sh", content = templatefile("${path.module}/scripts/ff-firewall.sh.tftpl", local.tpl) },
      { path = "/etc/ff/ff-secrets.sh", content = templatefile("${path.module}/scripts/ff-secrets.sh.tftpl", local.tpl) },
      { path = "/etc/ff/run-egress-proxy.sh", content = templatefile("${path.module}/scripts/run-egress-proxy.sh.tftpl", local.tpl) },
      { path = "/etc/ff/ff-image-watch.sh", content = file("${path.module}/scripts/ff-image-watch.sh") },
      { path = "/etc/ff/egress-proxy.env", content = join("", [for k in sort(keys(local.proxy_env)) : "${k}=${local.proxy_env[k]}\n"]) },
    ],
    [for n, u in local.units : { path = "/etc/systemd/system/${n}.service", content = u }],
  )

  # COS runs user-data through cloud-init on every boot (/etc is not
  # persistent), so the units are rewritten and started each time.
  cloud_init = "#cloud-config\n${yamlencode({
    write_files = [for f in local.files : {
      path        = f.path
      permissions = "0644"
      owner       = "root"
      encoding    = "b64"
      content     = base64encode(f.content)
    }]
    runcmd = [
      ["systemctl", "daemon-reload"],
      ["systemctl", "start", "ff-firewall.service", "ff-secrets.service", "ff-egress-proxy.service", "ff-image-watch.service"],
    ]
  })}"
}

resource "google_compute_instance" "this" {
  project      = var.project_id
  name         = "${var.name}-egress"
  zone         = var.zone
  machine_type = var.machine_type
  description  = "Flowforge egress proxy (HTTP/CONNECT, SOCKS5, DNS)"
  tags         = concat(["ff-egress-vm"], var.network_tags)
  labels       = var.labels

  allow_stopping_for_update = true
  deletion_protection       = var.deletion_protection

  boot_disk {
    auto_delete = true
    initialize_params {
      image  = data.google_compute_image.cos.self_link
      size   = var.boot_disk_gb
      type   = var.boot_disk_type
      labels = var.labels
    }
  }

  network_interface {
    subnetwork = var.subnetwork_id
    network_ip = google_compute_address.this.address

    dynamic "access_config" {
      for_each = var.external_ip ? [1] : []
      content {}
    }
  }

  service_account {
    email  = google_service_account.this.email
    scopes = ["cloud-platform"]
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  metadata = {
    user-data                 = local.cloud_init
    google-logging-enabled    = tostring(var.logging_enabled)
    google-monitoring-enabled = "false"
    enable-oslogin            = "TRUE"
    block-project-ssh-keys    = "TRUE"
    # Rolled forward by the deploy workflow; ff-image-watch restarts the
    # proxy when it changes.
    ff-egress-proxy-image = var.image
  }

  lifecycle {
    ignore_changes = [
      metadata["ff-egress-proxy-image"],
      # A new cos-stable image would otherwise replace the VM on every apply.
      boot_disk[0].initialize_params[0].image,
    ]
  }

  depends_on = [
    google_secret_manager_secret_iam_member.secrets,
    google_artifact_registry_repository_iam_member.reader,
  ]
}

# ---------------------------------------------------------------- firewall

resource "google_compute_firewall" "tools" {
  project       = var.project_id
  name          = "${var.name}-egress-allow-tools"
  network       = var.network_id
  direction     = "INGRESS"
  priority      = 1000
  target_tags   = ["ff-egress-vm"]
  source_ranges = var.tool_cidrs
  description   = "Egress proxy HTTP/CONNECT, SOCKS5 and DNS from the tool-runner subnet only"

  allow {
    protocol = "tcp"
    ports    = [tostring(var.proxy_port), tostring(var.socks_port), tostring(var.dns_port)]
  }

  allow {
    protocol = "udp"
    ports    = [tostring(var.dns_port)]
  }
}

resource "google_compute_firewall" "http_clients" {
  count         = length(var.http_client_cidrs) > 0 ? 1 : 0
  project       = var.project_id
  name          = "${var.name}-egress-allow-http"
  network       = var.network_id
  direction     = "INGRESS"
  priority      = 1000
  target_tags   = ["ff-egress-vm"]
  source_ranges = var.http_client_cidrs
  description   = "Egress proxy HTTP port (notify path) from the services subnet; no SOCKS5 or DNS"

  allow {
    protocol = "tcp"
    ports    = [tostring(var.proxy_port)]
  }
}

resource "google_compute_firewall" "iap_ssh" {
  count         = var.iap_ssh ? 1 : 0
  project       = var.project_id
  name          = "${var.name}-egress-allow-iap-ssh"
  network       = var.network_id
  direction     = "INGRESS"
  priority      = 1000
  target_tags   = ["ff-egress-vm"]
  source_ranges = ["35.235.240.0/20"]
  description   = "SSH through Identity-Aware Proxy only"

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# The tool-runner's default-deny egress (network module) gets one
# exception: the proxy's ports on this address.
resource "google_compute_firewall" "tools_to_proxy" {
  project            = var.project_id
  name               = "${var.name}-tools-allow-egress-vm"
  network            = var.network_id
  direction          = "EGRESS"
  priority           = 1000
  target_tags        = [var.tool_runner_tag]
  destination_ranges = ["${google_compute_address.this.address}/32"]

  allow {
    protocol = "tcp"
    ports    = [tostring(var.proxy_port), tostring(var.socks_port), tostring(var.dns_port)]
  }

  allow {
    protocol = "udp"
    ports    = [tostring(var.dns_port)]
  }
}
