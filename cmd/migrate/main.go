// Command migrate applies the database migrations (spec 10.7, the
// ff-migrate Cloud Run job) to FF_DB_DSN.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/genuinebnt/flowforge/services/app"
)

var version = "dev"

func main() {
	_ = version
	if err := app.Migrate(context.Background(), os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}
