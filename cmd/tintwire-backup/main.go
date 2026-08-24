package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tintwire-backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	database := flags.String("db", "tintwire.db", "source SQLite database")
	destination := flags.String("out", "", "new snapshot path")
	verify := flags.String("verify", "", "verify an existing snapshot instead of creating one")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if *verify != "" {
		if err := store.VerifyDatabase(ctx, *verify); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "verified %s\n", *verify)
		return 0
	}
	if *destination == "" {
		fmt.Fprintln(stderr, "-out is required when creating a backup")
		return 2
	}
	data, err := store.Open(*database)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer data.Close()
	if err := data.Backup(ctx, *destination); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "created and verified %s\n", *destination)
	return 0
}
