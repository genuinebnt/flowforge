// Command migrate applies the database migrations (spec 10.7, the
// ff-migrate Cloud Run job) to FF_DB_DSN. "migrate maintain" instead runs
// one maintenance pass (retention, partitions, run archival; see
// services/maintenance), for cron or a Cloud Run job.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/genuinebnt/flowforge/services/app"
)

// version is stamped at link time by deploy/Dockerfile
// (-ldflags "-X main.version=..."); nothing reports it yet, the blank
// use in main keeps the linker from dropping it.
var version = "dev"

func main() {
	_ = version
	run := app.Migrate
	if len(os.Args) > 1 && os.Args[1] == "maintain" {
		run = app.Maintain
	}
	if err := run(context.Background(), os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}
