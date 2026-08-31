package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kilo666mj/tintwire/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, store.MigrateSQLiteToPostgres))
}

func run(args []string, stdout, stderr io.Writer, migrate func(context.Context, string, string) error) int {
	flags := flag.NewFlagSet("tintwire-migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sqlitePath := flags.String("sqlite", "", "path to the source Tintwire SQLite database")
	postgresDSN := flags.String("postgres", "", "destination PostgreSQL connection string")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *sqlitePath == "" || *postgresDSN == "" {
		flags.Usage()
		return 2
	}
	if err := migrate(context.Background(), *sqlitePath, *postgresDSN); err != nil {
		_, _ = fmt.Fprintln(stderr, "migration failed:", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "SQLite data migrated and verified successfully")
	return 0
}
