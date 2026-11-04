package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// These commands write their result to stdout. Logs go to stderr so a pipe
	// captures only that output.
	logOut := os.Stdout
	switch os.Args[1] {
	case "admin-token", "restore", "restore-tokens", "backup", "gc":
		logOut = os.Stderr
	}
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(ctx, os.Getenv, nil, log)
	case "admin-token":
		err = runAdminToken(ctx, os.Getenv, os.Args[2:], os.Stdout, log)
	case "restore":
		err = runRestore(ctx, os.Getenv, os.Args[2:], os.Stdout, log)
	case "restore-tokens":
		err = runRestoreTokens(ctx, os.Getenv, os.Args[2:], os.Stdout, log)
	case "backup":
		err = runBackup(ctx, os.Getenv, os.Args[2:], os.Stdout, log)
	case "gc":
		err = runGC(ctx, os.Getenv, os.Args[2:], os.Stdout, log)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "littlekittyvcs: %s\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: littlekittyvcs serve\n       littlekittyvcs admin-token --label <text>\n       littlekittyvcs restore [--repo <name>]\n       littlekittyvcs restore-tokens\n       littlekittyvcs backup\n       littlekittyvcs gc [--dry-run] [--backup]\n")
}
