// Command testdb creates and drops throwaway local databases for the test
// and benchmark runners.
//
// It exists because the old suite reused one shared database: interrupted
// runs left project_test_* schemas behind, and enough of them turned simple
// statements into 30-second timeouts. A unique database per run removes the
// shared state entirely; the name prefix and local-host guard make it hard to
// point at anything that is not disposable.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var prefixRe = regexp.MustCompile(`^[a-z][a-z0-9_]{2,30}$`)
var dbNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}$`)

type output struct {
	Name       string `json:"name"`
	AdminURL   string `json:"admin_url"`
	RuntimeURL string `json:"runtime_url"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("testdb", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		action     = fs.String("action", "create", "create or drop")
		adminURL   = fs.String("admin-url", os.Getenv("TEST_DATABASE_ADMIN_URL"), "local admin DSN")
		runtimeURL = fs.String("runtime-url", os.Getenv("TEST_DATABASE_URL"), "local runtime DSN (optional)")
		prefix     = fs.String("prefix", "agents_proxy_test", "database name prefix")
		name       = fs.String("name", "", "database name for drop (or create override)")
		jsonOut    = fs.Bool("json", false, "emit JSON")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *adminURL == "" {
		fmt.Fprintln(stderr, "testdb: -admin-url or TEST_DATABASE_ADMIN_URL is required")
		return 2
	}
	if !prefixRe.MatchString(*prefix) {
		fmt.Fprintf(stderr, "testdb: invalid prefix %q\n", *prefix)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch *action {
	case "create":
		out, err := create(ctx, *adminURL, *runtimeURL, *prefix, *name)
		if err != nil {
			fmt.Fprintln(stderr, "testdb create:", err)
			return 1
		}
		if *jsonOut {
			_ = json.NewEncoder(stdout).Encode(out)
		} else {
			fmt.Fprintf(stdout, "TEST_DATABASE_ADMIN_URL=%s\nTEST_DATABASE_URL=%s\nTEST_DATABASE_NAME=%s\n",
				shellQuote(out.AdminURL), shellQuote(out.RuntimeURL), shellQuote(out.Name))
		}
	case "drop":
		if *name == "" {
			fmt.Fprintln(stderr, "testdb drop: -name is required")
			return 2
		}
		if err := drop(ctx, *adminURL, *prefix, *name); err != nil {
			fmt.Fprintln(stderr, "testdb drop:", err)
			return 1
		}
		if *jsonOut {
			_ = json.NewEncoder(stdout).Encode(map[string]string{"dropped": *name})
		} else {
			fmt.Fprintf(stdout, "dropped %s\n", *name)
		}
	default:
		fmt.Fprintf(stderr, "testdb: unknown action %q\n", *action)
		return 2
	}
	return 0
}

func create(ctx context.Context, adminURL, runtimeURL, prefix, requested string) (output, error) {
	admin, err := parseLocalURL(adminURL)
	if err != nil {
		return output{}, fmt.Errorf("admin url: %w", err)
	}
	name := requested
	if name == "" {
		name = freshName(prefix)
	}
	if !strings.HasPrefix(name, prefix+"_") {
		return output{}, fmt.Errorf("database name %q must start with %q_", name, prefix)
	}
	if len(name) > 63 {
		return output{}, fmt.Errorf("database name %q is longer than 63 bytes", name)
	}
	if !dbNameRe.MatchString(name) {
		return output{}, fmt.Errorf("database name %q is not a safe identifier", name)
	}

	conn, err := pgx.Connect(ctx, maintenanceURL(admin).String())
	if err != nil {
		return output{}, fmt.Errorf("connect maintenance database: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return output{}, fmt.Errorf("create database %s: %w", name, err)
	}

	runtime := admin
	if strings.TrimSpace(runtimeURL) != "" {
		runtime, err = parseLocalURL(runtimeURL)
		if err != nil {
			_ = drop(context.Background(), adminURL, prefix, name)
			return output{}, fmt.Errorf("runtime url: %w", err)
		}
	} else {
		runtime.User = url.User("app_runtime")
	}
	runtime.Path = "/" + name
	runtime.RawPath = ""
	admin.Path = "/" + name
	admin.RawPath = ""

	return output{Name: name, AdminURL: admin.String(), RuntimeURL: runtime.String()}, nil
}

func drop(ctx context.Context, adminURL, prefix, name string) error {
	if !strings.HasPrefix(name, prefix+"_") || !dbNameRe.MatchString(name) {
		return fmt.Errorf("refusing to drop %q: name must start with %q_", name, prefix)
	}
	admin, err := parseLocalURL(adminURL)
	if err != nil {
		return fmt.Errorf("admin url: %w", err)
	}
	conn, err := pgx.Connect(ctx, maintenanceURL(admin).String())
	if err != nil {
		return fmt.Errorf("connect maintenance database: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

func parseLocalURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, fmt.Errorf("scheme must be postgres or postgresql")
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "localhost", "127.0.0.1", "::1":
	default:
		return nil, fmt.Errorf("host %q is not local; refusing to create or drop a database", host)
	}
	if u.Path == "" || u.Path == "/" {
		return nil, fmt.Errorf("database name is missing")
	}
	return u, nil
}

func maintenanceURL(u *url.URL) *url.URL {
	cp := *u
	cp.Path = "/postgres"
	cp.RawPath = ""
	q := cp.Query()
	q.Del("pgbouncer")
	cp.RawQuery = q.Encode()
	return &cp
}

func freshName(prefix string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("testdb: crypto/rand: " + err.Error())
	}
	return fmt.Sprintf("%s_%s_%s", prefix, time.Now().UTC().Format("20060102_150405"), hex.EncodeToString(b[:]))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
