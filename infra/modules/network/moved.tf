# The NAT, private services access and private DNS resources became optional
# (count) for the lab environment; these keep existing dev/prod state in place.

moved {
  from = google_compute_global_address.psa
  to   = google_compute_global_address.psa[0]
}

moved {
  from = google_service_networking_connection.psa
  to   = google_service_networking_connection.psa[0]
}

moved {
  from = google_compute_router.router
  to   = google_compute_router.router[0]
}

moved {
  from = google_compute_router_nat.egress
  to   = google_compute_router_nat.egress[0]
}

moved {
  from = google_dns_managed_zone.googleapis
  to   = google_dns_managed_zone.googleapis[0]
}

moved {
  from = google_dns_record_set.googleapis_a
  to   = google_dns_record_set.googleapis_a[0]
}

moved {
  from = google_dns_record_set.googleapis_cname
  to   = google_dns_record_set.googleapis_cname[0]
}

moved {
  from = google_dns_managed_zone.run_app
  to   = google_dns_managed_zone.run_app[0]
}

moved {
  from = google_dns_record_set.run_app_a
  to   = google_dns_record_set.run_app_a[0]
}

moved {
  from = google_compute_firewall.tools_allow_redis
  to   = google_compute_firewall.tools_allow_redis[0]
}
