// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package jacklet builds the scraper, the Torznab API and the admin panel
// together over one store and one settings source, for a program that embeds
// them.
//
// The three parts have to share storage: what the scraper finds must land
// where the API and the panel read, and a setting the panel saves must be
// the one the scraper reads. Built separately, nothing stops them being given
// different stores, and the failure is a scrape whose results never appear.
// New takes one Store and one settings source and hands each part the same
// object, so the mismatch cannot be written.
//
// The package imports no storage package. A program using SQLite passes a
// *database.Store; one with its own storage passes that.
package jacklet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"

	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// Store is one object that plays every storage role: the Sink the scraper
// writes into, and what the Torznab API and the admin panel read from.
// *database.Store, the SQLite store Jacklet ships, satisfies it.
type Store interface {
	scraper.Sink
	admin.Store
}

// Options configures the parts New builds. Each field is that part's own
// options, except where its documentation says otherwise.
type Options struct {
	// APIKey is the "apikey" query parameter the Torznab endpoints require.
	// Empty leaves them open to anyone who can reach the server.
	APIKey string
	// Admin configures the panel: its mount path, branding and cookies.
	Admin admin.Options
	// AdminPassword is the panel's single credential. Nil, or one with
	// nothing configured, leaves the panel disabled.
	AdminPassword *admin.Password
	// FlareSolverrURL is the address of a FlareSolverr instance, which a
	// tracker uses once its own settings turn it on. Empty means none.
	FlareSolverrURL string
	// Scraper configures the scraper. Its Sink is set to the store, so it
	// must be left unset.
	Scraper scraper.Options
	// Torznab configures the API: its links and advertised contact.
	Torznab torznab.Options
}

// Stack is the parts New built, sharing one store and one settings source.
type Stack struct {
	// Admin is the panel, disabled when no password was given.
	Admin *admin.Admin
	// Scraper is shared by the API and the panel, and should live as long as
	// the program: it holds the sessions that make repeat searches cheap.
	Scraper *scraper.Scraper
	// Torznab is the API.
	Torznab *torznab.Torznab
}

// New builds the stack. store receives every scrape's results and is what the
// API and the panel read; config is where the scraper reads per-tracker
// settings and the panel edits them; trackers supplies the definitions.
func New(store Store, config admin.Config, trackers scraper.TrackerSource, logger *slog.Logger, options Options) (*Stack, error) {
	switch {
	case store == nil:
		return nil, errors.New("a store is required")
	case config == nil:
		return nil, errors.New("a settings source is required")
	case trackers == nil:
		return nil, errors.New("a definition source is required")
	case options.Scraper.Sink != nil:
		return nil, errors.New("the scraper options must leave Sink unset: it is set from the store")
	}

	scraperOptions := options.Scraper
	scraperOptions.Sink = store
	scrpr := scraper.NewWithOptions(config, options.FlareSolverrURL, logger, scraperOptions)

	panel, err := admin.NewWithOptions(store, scrpr, config, trackers, options.AdminPassword, logger, options.Admin)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize the admin panel: %w", err)
	}

	return &Stack{
		Admin:   panel,
		Scraper: scrpr,
		Torznab: torznab.NewWithOptions(store, scrpr, trackers, options.APIKey, logger, options.Torznab),
	}, nil
}

// Routes registers the API's endpoints and, when the panel is enabled, the
// panel's on mux. A route naming a path mux already serves makes ServeMux
// panic with a pattern conflict, which is returned here as the error it is;
// mux may then hold some of the routes and should be discarded.
func (s *Stack) Routes(mux *http.ServeMux) error {
	if err := register("the API under /api/v2.0/", func() { s.Torznab.Routes(mux) }); err != nil {
		return err
	}
	if !s.Admin.Enabled() {
		return nil
	}
	return register(fmt.Sprintf("admin prefix %q", s.Admin.Prefix()), func() { s.Admin.Routes(mux) })
}

// register runs routes, turning the panic of a conflicting pattern into an
// error naming what collided. A runtime error is a bug rather than a
// collision, so it is panicked again.
func register(what string, routes func()) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if runtimeErr, ok := recovered.(runtime.Error); ok {
				panic(runtimeErr)
			}
			err = fmt.Errorf("%s: a route collides with a path the server already serves: %v", what, recovered)
		}
	}()
	routes()
	return nil
}

// Handler returns the API and the panel as one http.Handler, for a program
// with no routes of its own to add. Mount it at the root: the API answers
// under /api/v2.0/ and the panel under its prefix, each at exactly the paths
// it registers.
func (s *Stack) Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	if err := s.Routes(mux); err != nil {
		return nil, err
	}
	return mux, nil
}

// Close releases what the scraper holds, destroying its FlareSolverr
// sessions, and should be called once at shutdown.
func (s *Stack) Close(ctx context.Context) error {
	return s.Scraper.Close(ctx)
}
