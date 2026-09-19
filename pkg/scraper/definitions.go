// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tracker represents a single torrent tracker definition.
type Tracker struct {
	Caps        Caps      `yaml:"caps"`
	Description string    `yaml:"description"`
	Encoding    string    `yaml:"encoding"`
	ID          string    `yaml:"id"`
	Language    string    `yaml:"language"`
	LegacyLinks []string  `yaml:"legacylinks"`
	Links       []string  `yaml:"links"`
	Login       *Login    `yaml:"login"`
	Name        string    `yaml:"name"`
	Replaces    []string  `yaml:"replaces"`
	Search      Search    `yaml:"search"`
	Settings    []Setting `yaml:"settings"`
	Type        string    `yaml:"type"`
}

// Setting represents a single user-configurable option (e.g. a checkbox or
// select) exposed by a definition, along with its default value. Without an
// override (see mergedConfig), a setting's default is what's used for its
// "{{ .Config.<name> }}" template value.
type Setting struct {
	Default any               `yaml:"default"`
	Label   string            `yaml:"label"`
	Name    string            `yaml:"name"`
	Options map[string]string `yaml:"options"`
	Type    string            `yaml:"type"`
}

// defaultConfig builds the ".Config" template context from a definition's
// settings, using each setting's default value.
func defaultConfig(def *Tracker) map[string]any {
	return mergedConfig(def, nil)
}

// mergedConfig builds the ".Config" template context from a definition's
// settings, using each setting's own default value unless overrides
// supplies a value under that setting's name. Everything becomes a string:
// a checkbox takes Cardigann's own two values, cardigannTrue and
// cardigannFalse, and anything else its string representation.
//
// A checkbox is not a Go bool, although "{{ if .Config.x }}" would read
// one correctly, because that is not all definitions do with it: comparing
// a setting against the ".False" literal — "{{ if eq .Config.x .False }}"
// — is a common shape, and Go's template "eq" refuses to compare a bool
// with the empty string that ".False" holds, failing the whole expression.
// Matching Jackett's representation makes both forms work.
func mergedConfig(def *Tracker, overrides map[string]any) map[string]any {
	cfg := make(map[string]any, len(def.Settings))
	for _, s := range def.Settings {
		val := s.Default
		if v, ok := overrides[s.Name]; ok {
			val = v
		}
		if s.Type == "checkbox" {
			if b, _ := val.(bool); b {
				cfg[s.Name] = cardigannTrue
			} else {
				cfg[s.Name] = cardigannFalse
			}
			continue
		}
		if val == nil {
			// A setting with no default, or an override written as a bare
			// "password:" with no value, is an empty string — formatting it
			// would otherwise yield the literal text "<nil>" and send that
			// to the tracker.
			cfg[s.Name] = ""
			continue
		}
		cfg[s.Name] = fmt.Sprintf("%v", val)
	}
	return cfg
}

// siteLinkSetting is the name Cardigann gives the base URL in ".Config".
// Jackett supplies it for every definition rather than expecting one to
// declare it as a setting, and definitions use it to build absolute URLs.
const siteLinkSetting = "sitelink"

// withSiteLink returns cfg with ".Config.sitelink" set to the mirror in
// use. It copies rather than writing through, since one resolved config is
// reused across a definition's mirrors and each has its own link.
//
// A definition that declares a "sitelink" setting of its own keeps it: the
// operator configured that deliberately, and Cardigann's own value is the
// fallback for the definitions — the large majority — that declare none.
func withSiteLink(cfg map[string]any, baseURL *url.URL) map[string]any {
	if existing, ok := cfg[siteLinkSetting]; ok && existing != "" {
		return cfg
	}
	merged := make(map[string]any, len(cfg)+1)
	maps.Copy(merged, cfg)
	merged[siteLinkSetting] = baseURL.String()
	return merged
}

// Login describes how a definition authenticates against its tracker.
// Credentials come from the definition's own "username"/"password"
// settings, which an operator overrides per tracker in CONFIG_DIR.
type Login struct {
	Captcha    *Captcha          `yaml:"captcha"`
	Error      []ErrorBlock      `yaml:"error"`
	Form       string            `yaml:"form"`
	Inputs     map[string]string `yaml:"inputs"`
	Method     string            `yaml:"method"`
	Path       string            `yaml:"path"`
	SubmitPath string            `yaml:"submitpath"`
	Test       *LoginTest        `yaml:"test"`
}

// Captcha marks a definition whose login is gated by a CAPTCHA. Only its
// presence carries meaning — the tracker is reported as unusable with a
// clear reason rather than failing as a generic login error — so the
// block's own keys are deliberately not modelled. Decoding a YAML mapping
// into this empty struct still yields a non-nil pointer, which is what
// presence detection relies on.
type Captcha struct{}

// LoginTest is the request a definition uses to check whether an existing
// session is still authenticated.
type LoginTest struct {
	Path     string `yaml:"path"`
	Selector string `yaml:"selector"`
}

// Caps represents the capabilities of a tracker.
type Caps struct {
	AllowRawSearch bool `yaml:"allowrawsearch"`
	// AllowTVSearchIMDB is Cardigann's "allowtvsearchimdb". Jackett sets
	// TvSearchImdbAvailable straight from it rather than deriving it from
	// the tv-search parameters, and the row-level "andmatch" filter gates
	// on that flag; see idSearchSupported.
	AllowTVSearchIMDB bool                `yaml:"allowtvsearchimdb"`
	CategoryMappings  []CategoryMapping   `yaml:"categorymappings"`
	Modes             map[string][]string `yaml:"modes"`
}

// CategoryMapping maps a tracker's category to a standard category.
type CategoryMapping struct {
	Cat string `yaml:"cat"`
	// Default marks a category as one to search when the client asked for
	// none. See DefaultCategoryIDs.
	Default bool       `yaml:"default"`
	ID      CategoryID `yaml:"id"`
}

// CategoryID is a tracker's own category identifier. Jackett treats it as
// an opaque string and its definitions spell it every way YAML allows — a
// bare number (id: 48), a quoted number (id: "1"), or a non-numeric name
// (id: tv) — so decoding it as a Go int rejects the whole definition file
// for a large share of Jackett's own definitions. It is compared as text
// against the category a row scrapes, which is likewise text.
type CategoryID string

// UnmarshalYAML implements yaml.Unmarshaler by taking any scalar node's
// text verbatim, so every spelling above decodes to the same id.
func (c *CategoryID) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("categorymappings id: expected a scalar, got kind %d", node.Kind)
	}
	*c = CategoryID(node.Value)
	return nil
}

// defaultSearchModes is what a definition is taken to support when it
// declares no caps.modes block: Jacklet can always drive a generic keyword
// search, and folds season/episode into the keywords for tv-search.
var defaultSearchModes = map[string][]string{
	"movie-search": {"q", "imdbid"},
	"search":       {"q"},
	"tv-search":    {"q", "season", "ep", "tvdbid"},
}

// SearchModes returns the search modes a definition supports, keyed by
// their Torznab caps element name ("search", "tv-search", "movie-search",
// "audio-search", "book-search") with the search parameters each mode
// accepts. A definition that declares no caps.modes block falls back to
// the modes Jacklet can serve from a generic keyword search.
func SearchModes(def *Tracker) map[string][]string {
	if len(def.Caps.Modes) == 0 {
		return cloneModes(defaultSearchModes)
	}

	modes := make(map[string][]string, len(def.Caps.Modes))
	for name, params := range def.Caps.Modes {
		if len(params) == 0 {
			params = []string{"q"}
		}
		modes[name] = slices.Clone(params)
	}
	return modes
}

// Search represents the search configuration for a tracker.
type Search struct {
	Error                []ErrorBlock      `yaml:"error"`
	Fields               FieldList         `yaml:"fields"`
	Headers              map[string]any    `yaml:"headers"`
	Inputs               map[string]string `yaml:"inputs"`
	KeywordsFilters      []Filter          `yaml:"keywordsfilters"`
	Paths                []SearchPath      `yaml:"paths"`
	PreprocessingFilters []Filter          `yaml:"preprocessingfilters"`
	Rows                 Rows              `yaml:"rows"`
}

// ErrorBlock declares how a definition recognizes a site-reported error on
// an otherwise successful response, so a "no permission" or "flood wait"
// page is surfaced instead of being scraped as zero results.
type ErrorBlock struct {
	Message  Field  `yaml:"message"`
	Selector string `yaml:"selector"`
}

// NamedField is one entry of a FieldList: a field's name paired with its
// definition, in the order it appeared in the YAML document.
type NamedField struct {
	Field Field
	Name  string
}

// FieldList decodes a YAML mapping of field name to Field while preserving
// declaration order. Order matters because a field's Text or a filter's
// args may reference an earlier field via "{{ .Result.<name> }}", and
// definitions rely on declaring dependencies before their use (a plain Go
// map would decode correctly but iterate in random order).
type FieldList []NamedField

// UnmarshalYAML implements yaml.Unmarshaler by walking the mapping node's
// key/value pairs in document order instead of decoding into a Go map.
func (f *FieldList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("fields: expected a YAML mapping, got kind %d", node.Kind)
	}

	result := make(FieldList, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		var field Field
		if err := node.Content[i+1].Decode(&field); err != nil {
			return fmt.Errorf("fields.%s: %w", node.Content[i].Value, err)
		}
		result = append(result, NamedField{Field: field, Name: node.Content[i].Value})
	}

	*f = result
	return nil
}

// SearchPath represents a search path.
type SearchPath struct {
	// Categories limits this path to searches touching these of the
	// tracker's own categories, so a definition can serve movies from one
	// page and music from another without fetching both every time. A
	// leading "!" negates the list. Empty means the path always runs.
	Categories []CategoryID `yaml:"categories"`
	// FollowRedirect makes this path's fetch follow a redirect instead of
	// scraping the redirect response itself. Cardigann defaults it off,
	// and a search that lands on a login page is the reason: following one
	// silently turns a lapsed session into an empty result set.
	FollowRedirect bool              `yaml:"followredirect"`
	InheritInputs  *bool             `yaml:"inheritinputs"`
	Inputs         map[string]string `yaml:"inputs"`
	Method         string            `yaml:"method"`
	Path           string            `yaml:"path"`
	Response       *Response         `yaml:"response"`
}

// negateCategories is the marker a search path's category list carries, as
// its first entry, to mean "every category except these".
const negateCategories = "!"

// selectCategories decides whether a search path runs for a search over
// the tracker categories in mapped, and which of them its ".Categories"
// then holds. A path declaring no categories always runs, over all of them.
//
// This mirrors Jackett, including one quirk worth stating rather than
// quietly improving on: a negated path that does run is given an *empty*
// ".Categories", because Jackett computes the intersection with its own
// category list in both branches and an intersection that just tested
// empty stays empty. No definition in Jackett's repository uses the
// negated form, so there is nothing depending on either reading — but a
// definition written against Jackett would have been tuned to that one.
func (p SearchPath) selectCategories(mapped []string) (categories []string, run bool) {
	if len(p.Categories) == 0 {
		return mapped, true
	}

	declared := make(map[string]bool, len(p.Categories))
	for _, c := range p.Categories {
		declared[string(c)] = true
	}

	var shared []string
	for _, c := range mapped {
		if declared[c] {
			shared = append(shared, c)
		}
	}

	match := len(shared) > 0
	if string(p.Categories[0]) == negateCategories {
		match = !match
	}
	if !match {
		return nil, false
	}
	return shared, true
}

// DefaultCategoryIDs returns the tracker's own category ids that its
// caps.categorymappings mark "default: true". Jackett searches these when
// the client asked for no category at all, which is what keeps a
// definition whose paths declare categories from matching none of them on
// an unfiltered search.
func DefaultCategoryIDs(def *Tracker) []string {
	var ids []string
	seen := make(map[CategoryID]bool)
	for _, mapping := range def.Caps.CategoryMappings {
		if mapping.Default && !seen[mapping.ID] {
			seen[mapping.ID] = true
			ids = append(ids, string(mapping.ID))
		}
	}
	return ids
}

// Response describes the format a search path returns. Cardigann defaults
// to HTML; "json" selects rows and fields by dotted key path instead of by
// CSS selector, and "xml" keeps the CSS selectors but reads them against
// an XML document — an RSS feed, in practice.
type Response struct {
	NoResultsMessage string `yaml:"noResultsMessage"`
	Type             string `yaml:"type"`
}

// IsJSON reports whether this path returns JSON rather than HTML.
func (p SearchPath) IsJSON() bool {
	return p.Response != nil && strings.EqualFold(p.Response.Type, "json")
}

// IsXML reports whether this path returns XML rather than HTML.
func (p SearchPath) IsXML() bool {
	return p.Response != nil && strings.EqualFold(p.Response.Type, "xml")
}

// InheritsInputs reports whether a path's own inputs are layered on top of
// the shared search.inputs block. Cardigann inherits by default.
func (p SearchPath) InheritsInputs() bool {
	return p.InheritInputs == nil || *p.InheritInputs
}

// Filter represents a filter to be applied to a value.
type Filter struct {
	Args any    `yaml:"args"`
	Name string `yaml:"name"`
}

// Rows represents the configuration for selecting rows from a search result.
type Rows struct {
	After       int    `yaml:"after"`
	DateHeaders *Field `yaml:"dateheaders"`
	// Filters are Cardigann's row filters, which decide whether a scraped
	// row is kept at all. They are not the value filters a Field carries:
	// see skipRow.
	Filters  []Filter `yaml:"filters"`
	Remove   string   `yaml:"remove"`
	Selector string   `yaml:"selector"`
}

// Field represents a field to be extracted from a search result.
type Field struct {
	Attribute string   `yaml:"attribute"`
	Case      CaseList `yaml:"case"`
	Filters   []Filter `yaml:"filters"`
	Optional  bool     `yaml:"optional"`
	Remove    string   `yaml:"remove"`
	Selector  string   `yaml:"selector"`
	Text      string   `yaml:"text"`
}

// CaseArm is one entry of a CaseList: a selector and the value to use when
// it matches. The selector "*" is the default arm.
type CaseArm struct {
	Selector string
	Value    string
}

// CaseList decodes a field's "case" mapping while preserving declaration
// order. Order is significant because the first matching arm wins and "*"
// is conventionally declared last as the fallback; a plain Go map would
// pick an arm at random.
type CaseList []CaseArm

// UnmarshalYAML implements yaml.Unmarshaler by walking the mapping node's
// key/value pairs in document order instead of decoding into a Go map.
func (c *CaseList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("case: expected a YAML mapping, got kind %d", node.Kind)
	}

	arms := make(CaseList, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		arms = append(arms, CaseArm{Selector: node.Content[i].Value, Value: node.Content[i+1].Value})
	}

	*c = arms
	return nil
}

// DefinitionError records one definition file that failed to load, without
// aborting the load of every other file in the directory.
type DefinitionError struct {
	Err  error
	Path string
}

func (e DefinitionError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

func (e DefinitionError) Unwrap() error {
	return e.Err
}

// ErrTrackerNotFound is returned by DefinitionStore.Find when no
// definition in the directory has the requested ID.
var ErrTrackerNotFound = errors.New("tracker not found")

// TrackerID returns the identifier a definition is stored and queried
// under: its "id" field, falling back to "name" for definitions that omit
// one.
func TrackerID(def *Tracker) string {
	if def.ID != "" {
		return def.ID
	}
	return def.Name
}
