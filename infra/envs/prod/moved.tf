# The egress proxy left Cloud Run for a VM (G-OPS-15, G-SVC-14). Its runtime
# account keeps the id sa-egress-proxy, so it moves instead of being
# destroyed and recreated under the same id; its Pub/Sub subscription grant
# (scope.events) follows it. The Cloud Run service itself is destroyed.
moved {
  from = module.svc["egress-proxy"].google_service_account.this
  to   = module.egress.google_service_account.this
}
