// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// settings is the resolved configuration for one run of the server.
type settings struct {
	APIKey            string
	AdminPassword     string
	AdminPasswordHash string
	BaseURL           string
	ConfigDir         string
	ContactEmail      string
	DBPath            string
	DefinitionsDir    string
	FlareSolverrURL   string
	LogFile           string
	Port              string
	RetentionDays     string
}

// secretEnvOnly names the credentials, which the environment carries. An
// argument is readable by every other process on the machine for as long
// as the server runs, and stays in shell history, which is what keeps
// `jacklet hash-password` on stdin too.
var secretEnvOnly = []struct{ name, purpose string }{
	{"API_KEY", "required apikey query parameter; unset leaves the indexer endpoints open"},
	{"ADMIN_PASSWORD", "plaintext password for the admin panel"},
	{"ADMIN_PASSWORD_HASH", "hashed admin password from `jacklet hash-password`; wins over the plaintext one"},
}

// newSettingsFlags builds the flag set and the settings it fills.
//
// Each flag defaults to the environment variable it mirrors, which gives
// the command line first say, the environment second, and the built-in
// default last.
func newSettingsFlags(out io.Writer) (*flag.FlagSet, *settings, *environment) {
	env := &environment{}
	cfg := &settings{}

	fs := flag.NewFlagSet("jacklet", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&cfg.BaseURL, "base-url", env.get("BASE_URL"), "external HTTP(S) origin used in generated links")
	fs.StringVar(&cfg.Port, "port", env.orDefault("PORT", "9117"), "port to listen on")
	fs.StringVar(&cfg.DefinitionsDir, "definitions-dir", definitionsDir(env.get("DEFINITIONS_DIR")), "directory of Cardigann indexer definitions")
	fs.StringVar(&cfg.ConfigDir, "config-dir", env.orDefault("CONFIG_DIR", "config"), "directory of per-indexer setting overrides")
	fs.StringVar(&cfg.ContactEmail, "contact-email", env.get("CONTACT_EMAIL"), "operator address advertised in the Torznab caps and feed; empty omits it")
	fs.StringVar(&cfg.DBPath, "db-path", env.orDefault("DB_PATH", defaultDBPath), "SQLite database file")
	fs.StringVar(&cfg.FlareSolverrURL, "flaresolverr-url", env.get("FLARESOLVERR_URL"), "FlareSolverr endpoint, for trackers behind an anti-bot challenge")
	fs.StringVar(&cfg.LogFile, "log-file", env.get("LOG_FILE"), "file to write logs to; empty logs to stdout")
	fs.StringVar(&cfg.RetentionDays, "retention-days", env.orDefault("RETENTION_DAYS", strconv.Itoa(defaultRetentionDays)), "days to keep a scraped torrent; 0 keeps everything")
	fs.Usage = func() { printUsage(out, fs) }

	return fs, cfg, env
}

// parseSettings resolves the configuration from args and the environment.
func parseSettings(args []string, out io.Writer) (*settings, error) {
	fs, cfg, env := newSettingsFlags(out)

	// Silenced so that every failure leaves as an error for the caller to
	// report, in one form.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	var err error
	for _, secret := range []struct {
		field *string
		name  string
	}{
		{&cfg.APIKey, "API_KEY"},
		{&cfg.AdminPassword, "ADMIN_PASSWORD"},
		{&cfg.AdminPasswordHash, "ADMIN_PASSWORD_HASH"},
	} {
		if *secret.field, err = env.secret(secret.name); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// printUsageTo writes the full help to out.
func printUsageTo(out io.Writer) {
	fs, _, _ := newSettingsFlags(out)
	fs.Usage()
}

// printUsage writes the full help: subcommands, flags, and the settings
// that only the environment carries.
func printUsage(out io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(out, usage)

	fmt.Fprintf(out, "\nOptions (each defaults to the matching %sVARIABLE):\n", envPrefix)
	previous := fs.Output()
	fs.SetOutput(out)
	fs.PrintDefaults()
	fs.SetOutput(previous)

	fmt.Fprintln(out, "\nEnvironment only, so the value stays out of the process list:")
	for _, env := range secretEnvOnly {
		fmt.Fprintf(out, "  %-28s %s\n", envPrefix+env.name, strings.ReplaceAll(env.purpose, "`", ""))
	}
	fmt.Fprint(out, "\nEach also reads a _FILE variant naming a file to read the value from,\nfor a Docker or Kubernetes secret mounted as one.\n")
}
