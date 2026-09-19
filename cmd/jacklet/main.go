// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Command jacklet serves scraped tracker listings over a Torznab API,
// wiring the scraper, the store, the Torznab handler and the admin panel
// into one process. It owns what those packages deliberately leave to a
// caller: configuration from flags and the JACKLET_* environment, the
// single long-lived Scraper every request shares, the retention sweep that
// keeps the store bounded, and graceful shutdown. It also carries the
// hash-password subcommand, so an operator can produce an admin password
// hash without the plaintext reaching their deployment configuration.
package main

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// publicFS holds the API reference page and OpenAPI document. They are
// embedded rather than read from disk so the binary serves them wherever
// it runs, without needing its working directory to be the source tree.
//
//go:embed public
var publicFS embed.FS

// defaultRetentionDays is how long a scraped torrent is kept, in days,
// unless RETENTION_DAYS says otherwise. Trackers are re-scraped
// continuously, so without a cutoff the store would grow without bound and
// keep serving long-dead releases.
const defaultRetentionDays = 30

// defaultDBPath is where the SQLite database lives unless DB_PATH or
// -db-path says otherwise.
const defaultDBPath = "jacklet.db"

// pruneInterval is how often expired torrents are swept out.
const pruneInterval = 1 * time.Hour

// retentionPeriod reads RETENTION_DAYS and converts it to a duration. Zero
// disables pruning altogether, for an operator who wants the store to keep
// everything it has ever scraped.
func retentionPeriod(value string) (time.Duration, error) {
	days, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid retention days %q: must be numeric", value)
	}
	if days < 0 {
		return 0, fmt.Errorf("invalid retention days %d: must not be negative", days)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// publicBaseURL validates the externally reachable origin advertised in
// generated links. A path would make endpoint construction ambiguous, and
// credentials in a URL should never be copied into responses.
func publicBaseURL(value string) (baseURL string, secure bool, err error) {
	if value == "" {
		return "", false, nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", false, fmt.Errorf("invalid base URL %q: must be an HTTP(S) origin without a path, query, fragment, or credentials", value)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if parsed.Host == "" || (scheme != "http" && scheme != "https") ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false, fmt.Errorf("invalid base URL %q: must be an HTTP(S) origin without a path, query, fragment, or credentials", value)
	}
	parsed.Scheme = scheme
	return strings.TrimRight(parsed.String(), "/"), scheme == "https", nil
}

// contactAddress validates the operator address advertised in the Torznab
// caps and feed. Only a bare address is accepted: the caps "email"
// attribute has no room for a display name, so "Ops <ops@example.org>"
// would reach a client verbatim and read as a broken address rather than
// as the contact it was meant to be. Empty advertises none.
//
// Surrounding whitespace is trimmed, since an environment file or compose
// entry carries it easily and it is never part of the address.
func contactAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value {
		return "", fmt.Errorf("invalid contact email %q: must be a bare address, such as ops@example.org", value)
	}
	return parsed.Address, nil
}

// healthCheckTimeout bounds how long a /healthz request waits on the
// database ping before reporting unhealthy.
const healthCheckTimeout = 5 * time.Second

// healthCheck returns a handler for container/orchestrator liveness and
// readiness probes: 200 if the database is reachable, 503 otherwise.
func healthCheck(store *database.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
		defer cancel()

		if err := store.Ping(ctx); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests to finish and cleanup (e.g. destroying a FlareSolverr session)
// to complete before exiting anyway.
const shutdownTimeout = 10 * time.Second

func main() {
	// A bare word is a subcommand; anything else is flags, apart from the
	// dashed spellings of version and help.
	args := os.Args[1:]
	if len(args) > 0 && (!strings.HasPrefix(args[0], "-") || isInfoFlag(args[0])) {
		if err := runCommand(args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	// Before the configuration is parsed, so that what an installed
	// service was configured with takes part in the ordinary precedence
	// rather than being applied on top of it afterwards. Warnings go to
	// stderr: there is no log yet, because where the log goes is one of
	// the things being settled here.
	applyServiceSettings(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := parseSettings(args, os.Stderr)
	if err != nil {
		// -h asks for the usage.
		if errors.Is(err, flag.ErrHelp) {
			printUsageTo(os.Stdout)
			return
		}
		fmt.Fprintf(os.Stderr, "%v\nRun \"jacklet --help\" for the available options.\n", err)
		os.Exit(1)
	}

	// Before the log is opened, because the log file is one of the paths
	// it settles.
	resolveServicePaths(cfg)

	// Structured JSON: the standard shape (time/level/msg plus attributes)
	// that container runtimes and log aggregators (Docker, Kubernetes,
	// Loki, CloudWatch, ...) expect a service to emit. It goes to stdout
	// unless a log file was configured, which lets the deployment
	// environment own log routing and retention wherever there is one.
	destination, closeLog, err := logDestination(cfg.LogFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(destination, nil))

	// Closed explicitly on both paths rather than deferred, since os.Exit
	// runs no deferred calls and a log file must be flushed either way.
	if err := runMain(logger, cfg); err != nil {
		logger.Error(err.Error())
		closeLog()
		os.Exit(1)
	}
	closeLog()
}

// defaultDefinitionsDirName is the directory definitions are read from,
// resolved beside the executable unless DEFINITIONS_DIR says otherwise.
const defaultDefinitionsDirName = "definitions"

// definitionsDir returns the directory to read definitions from:
// configured as given, else "definitions" beside the executable.
//
// Resolving against the executable keeps an unpacked release working from
// any working directory, so a service manager needs no WorkingDirectory.
// It also makes `go run` a special case: the binary it builds sits in a
// temporary directory, so a checkout needs DEFINITIONS_DIR. When the
// executable's own path is unavailable the bare name is all that is left
// to return, and that one resolves against the working directory.
func definitionsDir(configured string) string {
	if configured != "" {
		return configured
	}

	exe, err := os.Executable()
	if err != nil {
		return defaultDefinitionsDirName
	}
	// A binary reached through a symlink resolves to where it actually
	// lives, which is where its definitions sit.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), defaultDefinitionsDirName)
}

// run starts the server and blocks until ctx is cancelled or the server
// fails. ready, when non-nil, is called once the listener is accepting
// connections; a service manager uses it to distinguish "started" from
// "still opening the database", and everything else passes nil.
func run(ctx context.Context, logger *slog.Logger, cfg *settings, ready func()) error {
	// Logged first, so a bug report's logs identify the exact build that
	// produced everything below.
	logger.Info("starting Jacklet", readBuildDetails().LogAttrs()...)

	portNum, err := strconv.Atoi(cfg.Port)
	if err != nil {
		return fmt.Errorf("invalid port %q: must be numeric", cfg.Port)
	}
	// Reconstruct the port from its parsed numeric value, so downstream
	// logging and addresses never carry unsanitized input.
	port := strconv.Itoa(portNum)

	flareSolverrURL := cfg.FlareSolverrURL
	definitionsPath := cfg.DefinitionsDir
	configDir := cfg.ConfigDir
	dbPath := cfg.DBPath
	apiKey := cfg.APIKey
	baseURL, secureCookies, err := publicBaseURL(cfg.BaseURL)
	if err != nil {
		return err
	}
	contactEmail, err := contactAddress(cfg.ContactEmail)
	if err != nil {
		return err
	}
	adminPassword, err := admin.NewPassword(cfg.AdminPassword, cfg.AdminPasswordHash)
	if err != nil {
		return fmt.Errorf("invalid JACKLET_ADMIN_PASSWORD_HASH: %w", err)
	}

	retention, err := retentionPeriod(cfg.RetentionDays)
	if err != nil {
		return err
	}

	store, err := database.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer store.Close()

	// One Scraper is shared across every request, so its HTTP client,
	// cookie jar, FlareSolverr session, and per-tracker rate limiting all
	// persist for the life of the process instead of being rebuilt per
	// search.
	// Per-tracker setting overrides, read by the scraper and edited by the
	// admin panel.
	config := scraper.NewConfigStore(configDir)
	scrpr := scraper.New(store, config, flareSolverrURL, logger)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := scrpr.Close(closeCtx); err != nil {
			logger.Error("error closing scraper", "error", err)
		}
	}()
	// Definitions are parsed once and re-read only when a file changes, so
	// they stay editable without a restart without costing every request a
	// full parse of the directory.
	trackers := scraper.NewDefinitionStore(definitionsPath, logger)
	warnIfNoDefinitions(trackers, definitionsPath, logger)
	torznabHandler := torznab.NewWithOptions(store, scrpr, trackers, apiKey, logger, torznab.Options{BaseURL: baseURL, ContactEmail: contactEmail})

	adminPanel, err := admin.NewWithOptions(store, scrpr, config, trackers, adminPassword, logger, admin.Options{SecureCookies: secureCookies})
	if err != nil {
		return fmt.Errorf("failed to initialize the admin panel: %w", err)
	}

	publicDir, err := fs.Sub(publicFS, "public")
	if err != nil {
		return fmt.Errorf("failed to open embedded assets: %w", err)
	}

	mux := http.NewServeMux()
	// Each tracker definition gets its own Torznab endpoint at the address
	// Jackett serves the same thing from, so a client already pointed at a
	// Jackett instance needs no reconfiguring. The reserved id
	// torznab.AggregateID ("all") queries every definition at once, for a
	// client that would rather add one endpoint than many.
	torznabHandler.Routes(mux)
	// Interactive API reference (Scalar), reading /openapi.yaml.
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, publicDir, "docs.html")
	})
	mux.HandleFunc("GET /healthz", healthCheck(store))
	// The panel reads and writes tracker credentials, so it is served only
	// when an admin password is configured; without one it answers 404.
	if adminPanel.Enabled() {
		adminPanel.Routes(mux)
	}
	// Redirect the bare root to the docs instead of falling through to the
	// static file server's directory listing of "public/" (it has no
	// index.html). "/{$}" matches only the exact root path, so every other
	// static asset (e.g. /openapi.yaml) still falls through to it below.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs", http.StatusFound)
	})
	// Serve the embedded static assets.
	mux.Handle("/", http.FileServerFS(publicDir))

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// A live search may legitimately take up to torznab.ScrapeTimeout
		// re-scraping a slow indexer before it can write a response; give
		// it headroom rather than cutting a genuinely slow-but-working
		// search off mid-response.
		WriteTimeout: torznab.ScrapeTimeout + 15*time.Second,
	}

	if retention > 0 {
		go pruneExpired(ctx, store, retention, pruneInterval, logger)
	} else {
		logger.Warn("JACKLET_RETENTION_DAYS is 0; scraped torrents are never pruned")
	}

	// Bound before the goroutine starts, so a port already in use is
	// returned as a startup error rather than reported asynchronously after
	// the caller has been told the server is ready.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", server.Addr, err)
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "port", port)
		if apiKey == "" {
			logger.Warn("JACKLET_API_KEY is not set; the indexer endpoints are unauthenticated")
		}
		if adminPanel.Enabled() {
			logger.Info("admin panel available", "path", "/admin", "hashedPassword", adminPassword.Hashed())
			if !adminPassword.Hashed() {
				logger.Warn("JACKLET_ADMIN_PASSWORD is set in plaintext; " +
					"set JACKLET_ADMIN_PASSWORD_HASH from \"jacklet hash-password\" to keep it out of your deployment config")
			}
		} else {
			logger.Info("JACKLET_ADMIN_PASSWORD is not set; the admin panel is disabled")
		}
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	if ready != nil {
		ready()
	}

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("server error: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("error shutting down server", "error", err)
	}
	return nil
}

// pruneExpired sweeps torrents older than retention out of the store every
// interval, until ctx is cancelled.
func pruneExpired(ctx context.Context, store *database.Store, retention, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := store.Prune(ctx, retention)
			if err != nil {
				logger.Error("failed to prune expired torrents", "error", err)
				continue
			}
			if removed > 0 {
				logger.Info("pruned expired torrents", "removed", removed)
			}
		}
	}
}

// usage describes the subcommands the binary accepts. Running it with no
// arguments starts the server, which is the ordinary case.
const usage = `jacklet - a Torznab indexer proxy

Usage:
  jacklet [options]       Start the server.
  jacklet hash-password   Read a password from stdin and print a hash for
                          JACKLET_ADMIN_PASSWORD_HASH, so the password
                          itself need not appear in your deployment
                          configuration.
  jacklet service         Manage the Windows service. Run it with no verb
                          for the available ones. Windows only.
  jacklet version         Print the version, revision and build time.
`

// isInfoFlag reports whether arg is a dashed spelling of the version or
// help subcommands, which are conventionally written with dashes. Handling
// them here puts every spelling on stdout, where a reader can page or
// redirect it.
func isInfoFlag(arg string) bool {
	switch arg {
	case "-v", "-version", "--version", "-h", "-help", "--help":
		return true
	default:
		return false
	}
}

// runCommand dispatches a subcommand.
func runCommand(args []string) error {
	switch args[0] {
	case "hash-password":
		return hashPassword(os.Stdin, os.Stdout, os.Stderr)
	case "service":
		return runService(args[1:])
	case "version", "-v", "-version", "--version":
		printVersion(os.Stdout)
		return nil
	case "help", "-h", "-help", "--help":
		printUsageTo(os.Stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

// hashPassword reads one line from in and writes its hash to out. The
// password is read from stdin rather than taken as an argument, because a
// command-line argument is visible to every other process on the machine.
func hashPassword(in io.Reader, out, errOut io.Writer) error {
	if f, ok := in.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintln(errOut, "Enter the password, then press Enter.")
			fmt.Fprintln(errOut, "Note: it will be visible as you type. To avoid that, pipe it in instead:")
			// Passed as an argument, not a format string: the hint itself
			// contains a printf directive.
			fmt.Fprintf(errOut, "  %s\n", `read -rs PASSWORD && printf '%s' "$PASSWORD" | jacklet hash-password`)
		}
	}

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return err
		}
		return errors.New("no password was provided on stdin")
	}

	hash, err := admin.HashPassword(strings.TrimRight(scanner.Text(), "\r\n"))
	if err != nil {
		return err
	}

	fmt.Fprintln(out, hash)
	return nil
}

// warnIfNoDefinitions tells the operator where to put indexer definitions
// when none are present. Jacklet ships no definitions of its own, so a
// fresh install serves an empty indexer list until some are supplied, and
// that is easier to diagnose from a startup line than from an empty
// response.
func warnIfNoDefinitions(trackers scraper.TrackerSource, dir string, logger *slog.Logger) {
	defs, _, err := trackers.Trackers()
	if err != nil {
		logger.Warn("could not read the definitions directory", "dir", dir, "error", err)
		return
	}
	if len(defs) == 0 {
		logger.Warn("no indexer definitions found; Jacklet has nothing to search",
			"dir", dir,
			"resolved", absPath(dir),
			"hint", "copy Cardigann YAML definitions from github.com/Jackett/Jackett (Definitions/) into this directory, or set JACKLET_DEFINITIONS_DIR")
		return
	}
	logger.Info("loaded indexer definitions", "dir", dir, "resolved", absPath(dir), "count", len(defs))

	// The aggregate endpoint owns that path segment, so a definition
	// claiming the same id is still searched through the aggregate but
	// cannot be reached on its own path — worth saying once at startup
	// rather than leaving as a silent 404-shaped surprise.
	for i := range defs {
		if scraper.TrackerID(&defs[i]) == torznab.AggregateID {
			logger.Warn("a definition uses the id reserved for the aggregate indexer; it is searchable only through the aggregate",
				"id", torznab.AggregateID,
				"name", defs[i].Name,
				"hint", "give the definition a different id to reach it on its own endpoint")
		}
	}
}

// absPath renders a path as an absolute one for logging, falling back to
// the original if the working directory cannot be determined.
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
