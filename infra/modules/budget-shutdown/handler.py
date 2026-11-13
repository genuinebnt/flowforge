# Budget-triggered shutdown (infra/modules/budget-shutdown). Runs as
#   python3 -c "<this file>"
# in a google-cloud-cli image on Cloud Run, behind a Pub/Sub push
# subscription on the budget's notification topic. Cloud Billing publishes a
# notification several times a day; when costAmount reaches
# budgetAmount * FF_SHUTDOWN_THRESHOLD the handler:
#   1. sets the gateway's ingress to internal (no public traffic),
#   2. pauses the Cloud Scheduler jobs (the schedules tick),
#   3. sets min instances 0 on the listed services,
#   4. stops the VM (lab) and the Cloud SQL instance (dev/prod),
# so Cloud Run scales to zero and the VM stops billing. Every step is
# idempotent; `make gcp-resume ENV=<env>` (or `terraform apply`) undoes it.
import base64
import json
import os
import subprocess
from http.server import BaseHTTPRequestHandler, HTTPServer

PROJECT = os.environ["FF_PROJECT"]
REGION = os.environ["FF_REGION"]
THRESHOLD = float(os.environ.get("FF_SHUTDOWN_THRESHOLD", "1.0"))


def log(severity, msg, **kw):
    print(json.dumps({"severity": severity, "msg": msg, **kw}), flush=True)


def gcloud(*args):
    cmd = ["gcloud", *args, "--project", PROJECT, "--quiet"]
    rc = subprocess.run(cmd).returncode
    log("NOTICE" if rc == 0 else "ERROR", "shutdown.step", cmd=" ".join(args), rc=rc)
    return rc


def items(name):
    return [v for v in os.environ.get(name, "").split(",") if v]


def shutdown():
    rc = 0
    gateway = os.environ.get("FF_GATEWAY_SERVICE", "")
    if gateway:
        rc |= gcloud("run", "services", "update", gateway, "--region", REGION, "--ingress", "internal")
    for job in items("FF_SCHEDULER_JOBS"):
        rc |= gcloud("scheduler", "jobs", "pause", job, "--location", REGION)
    for svc in items("FF_SCALE_DOWN_SERVICES"):
        rc |= gcloud("run", "services", "update", svc, "--region", REGION, "--min-instances", "0")
    vm = os.environ.get("FF_VM", "")
    if vm:
        rc |= gcloud("compute", "instances", "stop", vm, "--zone", os.environ["FF_VM_ZONE"], "--async")
    sql = os.environ.get("FF_SQL_INSTANCE", "")
    if sql:
        rc |= gcloud("sql", "instances", "patch", sql, "--activation-policy", "NEVER", "--async")
    return rc


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        try:
            body = json.loads(self.rfile.read(int(self.headers.get("content-length", "0"))) or b"{}")
            note = json.loads(base64.b64decode(body["message"]["data"]))
            cost = float(note.get("costAmount", 0))
            budget = float(note.get("budgetAmount", 0))
        except Exception as err:  # malformed: ack so Pub/Sub does not retry forever
            log("WARNING", "shutdown.bad_message", err=str(err))
            self.send_response(204)
            self.end_headers()
            return
        trip = budget > 0 and cost >= budget * THRESHOLD
        log("NOTICE" if trip else "INFO", "shutdown.budget", cost=cost, budget=budget, threshold=THRESHOLD, trip=trip)
        code = 204
        if trip and shutdown() != 0:
            code = 500  # Pub/Sub redelivers; the steps are idempotent
        self.send_response(code)
        self.end_headers()

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok\n")

    def log_message(self, *args):
        pass


HTTPServer(("", int(os.environ.get("PORT", "8080"))), Handler).serve_forever()
