// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/araddon/dateparse"
	"golang.org/x/text/unicode/norm"
)

// templateData is the top-level context available to Cardigann template
// expressions ("{{ ... }}") found in search inputs, field text, and filter
// arguments. Its shape mirrors the subset of Jackett/Cardigann's own
// template variables Jacklet supports: .Config (settings, defaulted or
// overridden), .Keywords (the search query, already folded with
// season/episode when applicable), .Categories (site-specific category IDs
// requested), .Query (the raw Torznab search parameters), .Today (the
// current date), .True/.False (Cardigann's boolean literals), and .Result
// (fields already extracted for the current row). Cardigann's ".Env" is
// deliberately not implemented: exposing the process environment to
// arbitrary, possibly third-party YAML definitions would let a definition
// exfiltrate secrets (e.g. JACKLET_API_KEY) into a scraped field that ends up
// stored and re-served over the Torznab API.
type templateData struct {
	Categories []string
	Config     map[string]any
	// DownloadUri is the link a download starts from, set only while a
	// definition's download block runs. Its name is Cardigann's.
	DownloadUri downloadURIVars
	// False is the empty string and True is "True", the values Jackett
	// gives these two variables. Definitions use them as literals a value
	// can be compared against — "{{ if eq .Query.IMDBID .False }}" is the
	// common shape — which is why False is empty rather than "False":
	// comparing against an unset parameter has to succeed, and an empty
	// string is also what "{{ if }}" treats as false.
	False    string
	Keywords string
	Query    queryParams
	Result   map[string]string
	Today    todayVars
	True     string

	// encoding is the definition's declared character set. It is
	// deliberately unexported: text/template cannot reach an unexported
	// field, so this rides along to the filters that need it without
	// becoming a variable a definition could reference.
	encoding string
}

// downloadURIVars is Cardigann's ".DownloadUri": the parts of a download
// link, under the names Jackett gives them, so a download block can build a
// request from the release id in its link.
type downloadURIVars struct {
	AbsolutePath string
	AbsoluteUri  string
	Host         string
	PathAndQuery string
	Port         string
	Query        uriQueryVars
	Scheme       string
}

// uriQueryVars is ".DownloadUri.Query": each query parameter's first value
// by name, as in "{{ .DownloadUri.Query.id }}", and the query string with
// its "?" where it is rendered whole.
type uriQueryVars map[string]string

func (q uriQueryVars) String() string {
	values := url.Values{}
	for key, value := range q {
		values.Set(key, value)
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}

// newDownloadURIVars takes link apart as .NET's Uri does: the query keeps
// its "?", and the port is the scheme's default when the link names none.
func newDownloadURIVars(link *url.URL) downloadURIVars {
	query := uriQueryVars{}
	for key, list := range link.Query() {
		query[key] = list[0]
	}
	pathAndQuery := link.EscapedPath()
	if link.RawQuery != "" {
		pathAndQuery += "?" + link.RawQuery
	}
	port := link.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[link.Scheme]
	}
	return downloadURIVars{
		AbsolutePath: link.EscapedPath(),
		AbsoluteUri:  link.String(),
		Host:         link.Hostname(),
		PathAndQuery: pathAndQuery,
		Port:         port,
		Query:        query,
		Scheme:       link.Scheme,
	}
}

// todayVars is Cardigann's ".Today". Jackett offers only ".Today.Year";
// rendering the whole variable is a Jacklet addition, and String is what
// a definition writing "{{ .Today }}" renders: the date itself.
type todayVars struct {
	// Year is the current year — except in January, when it is the
	// previous one, as it is in Jackett. Definitions reach for it to
	// complete a date the tracker prints without a year, and in January a
	// listing without a year is far more likely to be from December than
	// from the last few days.
	Year string
	date string
}

func (t todayVars) String() string { return t.date }

// newTodayVars builds ".Today" from a point in time. The clock is the
// caller's so a test can pin it.
func newTodayVars(now time.Time) todayVars {
	year := now.Year()
	if now.Month() == time.January {
		year--
	}
	return todayVars{Year: strconv.Itoa(year), date: now.Format("2006-01-02")}
}

// cardigannTrue and cardigannFalse are the values Jackett gives ".True"
// and ".False", and the two states a checkbox setting takes in ".Config"
// (see mergedConfig).
const (
	cardigannFalse = ""
	cardigannTrue  = "True"
)

// queryParams mirrors Cardigann's ".Query.*" template variables: the raw
// Torznab search parameters, made available to definitions that support
// searching by season/episode, by external ID or by page directly,
// separately from whatever text ends up in .Keywords.
//
// The field set matches the one Jackett builds, so a definition written
// against Jackett renders here rather than failing: Go's text/template
// treats a missing field as an error, and renderTemplate then leaves the
// string unresolved, which would send the tracker a literal
// "{{ .Query.IsTVSearch }}". The one variable deliberately left out is
// Jackett's ".Query.APIKey" — that is Jacklet's own API key, and putting
// it within reach of a third-party YAML definition is the same
// secret-exfiltration risk that keeps ".Env" unimplemented.
//
// Series and Movie are always empty, as they are in Jackett.
type queryParams struct {
	Album string
	// APIKey is absent on purpose; see the type comment.
	Artist string
	Author string
	// Categories are the standard Torznab category ids the client asked
	// for, as Jackett sets them. They are deliberately *not* the tracker's
	// own ids: the top-level ".Categories" holds those, reverse-mapped
	// through caps.categorymappings, and a definition building a request
	// out of site categories wants that one.
	Categories []string
	DoubanID   string
	Ep         string
	// Episode is the season/episode as one "SxxEyy" string, Jackett's
	// GetEpisodeSearchString(). Empty when neither was given.
	Episode  string
	Extended string
	Genre    string
	IMDBID   string
	// IMDBIDShort is IMDBID without its "tt" prefix, which is the form
	// some trackers expect in a query parameter.
	IMDBIDShort string
	// The Is* flags classify the request the way Jackett's query object
	// does, for definitions that branch on the kind of search rather than
	// on individual parameters. Each is "True" or empty, so it reads as a
	// boolean in a template's "{{ if }}".
	IsBookSearch  string
	IsDoubanQuery string
	IsGenreQuery  string
	IsIdSearch    string
	IsImdbQuery   string
	IsMovieSearch string
	IsMusicSearch string
	IsRssSearch   string
	IsSearch      string
	IsTVRageQuery string
	IsTVSearch    string
	// Keywords is the search text before the definition's
	// keywordsfilters run; the top-level ".Keywords" is the same value
	// with them applied. A definition reaches for this one when it wants
	// the terms the client actually sent, having rewritten them for its
	// own search box in .Keywords.
	Keywords string
	Label    string
	// Limit is the client's page size, for a tracker that pages
	// server-side.
	Limit string
	// Movie is always empty, matching Jackett.
	Movie string
	// Offset is the client's paging offset, for a tracker that pages
	// server-side.
	Offset    string
	Publisher string
	// Q is the raw "q" parameter, unlike .Keywords, which has the other
	// search terms folded in and the definition's keywordsfilters applied.
	Q      string
	Season string
	// Series is always empty, matching Jackett.
	Series   string
	TMDBID   string
	TVDBID   string
	TVMazeID string
	TVRageID string
	Title    string
	Track    string
	TraktID  string
	Type     string
	Year     string
}

// cardigannFuncs are Cardigann's two template functions. Jackett applies
// them by regex substitution over the template text rather than through a
// template engine, so they are functions here only because Go has one —
// the call syntax a definition writes is the same either way.
var cardigannFuncs = template.FuncMap{
	"join":       templateJoin,
	"re_replace": templateReReplace,
}

// lenientFuncs and strictFuncs are cardigannFuncs with the lookup a
// hyphenated key is rewritten into, for a lenient and a strict render, and
// the encoder renderSearchPath ends each action with.
var (
	lenientFuncs = withKeyFunc(false)
	strictFuncs  = withKeyFunc(true)
)

// withKeyFunc returns cardigannFuncs with mapKeyFunc(isStrict) added as
// "cardigannKey".
func withKeyFunc(isStrict bool) template.FuncMap {
	funcs := maps.Clone(cardigannFuncs)
	funcs["cardigannKey"] = mapKeyFunc(isStrict)
	funcs["pathValue"] = pathValue
	return funcs
}

// mapKeyFunc returns the lookup a hyphenated key is rewritten into: the
// value under key in m, a map of the template context. A key m lacks is
// an error when isStrict, matching a strict template's missingkey=error,
// and otherwise the map's zero value, as the builtin index returns it.
func mapKeyFunc(isStrict bool) func(m any, key string) (any, error) {
	return func(m any, key string) (any, error) {
		v := reflect.ValueOf(m)
		if v.Kind() != reflect.Map {
			return nil, fmt.Errorf("cannot look up key %q in %T", key, m)
		}
		value := v.MapIndex(reflect.ValueOf(key))
		if value.IsValid() {
			return value.Interface(), nil
		}
		if isStrict {
			return nil, fmt.Errorf("map has no entry for key %q", key)
		}
		return reflect.Zero(v.Type().Elem()).Interface(), nil
	}
}

// templateJoin is Cardigann's "join", which concatenates a list variable —
// in practice always ".Categories" — with a separator.
func templateJoin(list any, sep string) string {
	switch v := list.(type) {
	case nil:
		return ""
	case []string:
		return strings.Join(v, sep)
	case string:
		// Not a list at all; joining one element is the element.
		return v
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = fmt.Sprintf("%v", item)
		}
		return strings.Join(parts, sep)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// templateReReplace is Cardigann's "re_replace" in its template-function
// form, the same substitution the filter of that name performs -- Jackett
// backs both with .NET's Regex.Replace, so the replacement string needs
// the same translation into Go's syntax (see goReplacement). An invalid
// pattern is reported rather than swallowed: the alternative is silently
// sending the tracker a value the definition meant to rewrite.
func templateReReplace(value any, pattern, replacement string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("re_replace: %w", err)
	}
	return re.ReplaceAllString(fmt.Sprintf("%v", value), goReplacement(replacement)), nil
}

// goStringEscapes are the characters that may follow a backslash in a Go
// string literal. Everything else is a syntax error there, while being
// perfectly ordinary in a regular expression.
const goStringEscapes = `abfnrtv\'"xuU01234567`

// hyphenatedMapKey matches a ".Config.cat-id" style chain: a lookup into
// one of the two template context maps whose key contains a hyphen. Only
// those two are maps, so a hyphen after anything else is a definition's
// own error rather than something to rewrite.
//
// A hyphen can never be an operator here: Go's template language has no
// infix arithmetic, so the parser reads ".Config.cat-id" as a subtraction
// it then rejects, and there is no valid expression this pattern could
// capture by mistake.
var hyphenatedMapKey = regexp.MustCompile(`^\.(Config|Result)\.([A-Za-z_]\w*(?:-\w+)+)`)

// rewriteCardigannTemplate adapts what a Cardigann definition writes to
// what Go's template parser accepts. It is the one place where reusing
// text/template instead of Cardigann's own engine costs something: Jackett
// applies templates by substituting over the text with a regex and so
// never parses them, which lets its definitions carry three shapes that a
// real parser rejects outright.
//
//   - A regular expression argument. Go lexes a template's string literals
//     by Go's rules, where "\s" is not an escape but a parse error, and
//     `{{ re_replace .Keywords "[\s]+" "%" }}` is the most common use of
//     that function in Jackett's definitions. Each backslash that does not
//     begin a valid Go escape is doubled, which is what the author meant
//     by it: a literal backslash. One that does begin a valid escape is
//     left alone, since a definition writing "\t" means a tab.
//
//   - A hyphenated setting name. `{{ .Config.cat-id }}` parses as a
//     subtraction, so it becomes `(cardigannKey .Config "cat-id")`, the
//     same lookup spelled in a way the parser accepts. It is a function of
//     Jacklet's own rather than the builtin index, because index answers a
//     missing key with an empty value whatever the template's missingkey
//     option says (see mapKeyFunc).
//
//   - A closing parenthesis with nothing open. 1337x writes
//     `(eq .Config.disablesort .False))`, which Jackett's substitution
//     reads as meant, since the pattern for eq takes one parenthesis along
//     with its last argument. The unmatched one is dropped; a template
//     carrying one never parses, so no template that parses is changed.
//
// The rewrites are confined to what they apply to: the escaping happens
// only inside a double-quoted argument, the others only outside any quoted
// argument, raw strings, character constants and comments included, and none
// touches the text between actions.
func rewriteCardigannTemplate(s string) string {
	var out strings.Builder
	out.Grow(len(s))

	inAction, inString := false, false
	openParens := 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case !inAction:
			if strings.HasPrefix(s[i:], "{{") {
				inAction = true
				openParens = 0
				out.WriteString("{{")
				i += 2
				continue
			}
			out.WriteByte(c)
			i++

		case inString:
			if c == '\\' && i+1 < len(s) {
				if next := s[i+1]; !strings.ContainsRune(goStringEscapes, rune(next)) {
					out.WriteString(`\\`)
					i++
					continue
				}
				// A valid escape is copied whole, so the second backslash
				// of `\\` is not mistaken for the start of another one.
				out.WriteString(s[i : i+2])
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			out.WriteByte(c)
			i++

		default: // inside an action, outside a quoted argument
			if strings.HasPrefix(s[i:], "}}") {
				inAction = false
				out.WriteString("}}")
				i += 2
				continue
			}
			if c == '"' {
				inString = true
				out.WriteByte(c)
				i++
				continue
			}
			i = rewriteActionAt(s, i, &out, &openParens)
		}
	}
	return out.String()
}

// rewriteActionAt writes what s[i] begins inside an action, outside a
// double-quoted argument, and returns where the next one begins: a raw
// string, a character constant or a comment as written, nothing for a closing
// parenthesis with nothing open, a hyphenated key as its lookup, and any
// other byte as it is.
func rewriteActionAt(s string, i int, out *strings.Builder, openParens *int) int {
	if end := literalEnd(s, i); end > i {
		out.WriteString(s[i:end])
		return end
	}
	if !keepsParen(s[i], openParens) {
		return i + 1
	}
	if m := hyphenatedKeyAt(s[i:]); m != nil {
		fmt.Fprintf(out, "(cardigannKey .%s %q)", m[1], m[2])
		return i + len(m[0])
	}
	out.WriteByte(s[i])
	return i + 1
}

// hyphenatedKeyAt matches hyphenatedMapKey at the start of s, looking no
// further than a first byte that cannot begin one.
func hyphenatedKeyAt(s string) []string {
	if !strings.HasPrefix(s, ".") {
		return nil
	}
	return hyphenatedMapKey.FindStringSubmatch(s)
}

// literalEnd returns where a raw string, a character constant or a comment
// starting at s[i] ends, or i when none starts there. Each is copied as
// written: a parenthesis inside one is text, none takes the escaping a
// double-quoted argument gets, and a quote inside a comment opens nothing.
func literalEnd(s string, i int) int {
	if strings.HasPrefix(s[i:], "/*") {
		if end := strings.Index(s[i+2:], "*/"); end >= 0 {
			return i + 2 + end + 2
		}
		return len(s)
	}
	switch s[i] {
	case '`':
		if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
			return i + 1 + end + 1
		}
		return len(s)
	case '\'':
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '\'':
				return j + 1
			}
		}
		return len(s)
	}
	return i
}

// keepsParen counts the parentheses an action opens and closes, reporting
// whether c is kept: a closing one with nothing open is not.
func keepsParen(c byte, openParens *int) bool {
	switch c {
	case '(':
		*openParens++
	case ')':
		if *openParens == 0 {
			return false
		}
		*openParens--
	}
	return true
}

// renderTemplate evaluates s as a Go text/template against data. Cardigann's
// template syntax ("{{ if }}", "{{ range }}", ".Config.x", "and"/"or"/"ne",
// etc.) is deliberately modeled on the same syntax as Go's text/template,
// so Jacklet reuses it directly instead of writing a bespoke parser.
// A string with no "{{" is returned unchanged without invoking the
// template engine. A malformed or failing template logs at debug level
// and returns s unresolved, rather than aborting the scrape.
func renderTemplate(s string, data templateData, logger *slog.Logger) string {
	rendered, err := executeTemplate(s, data, false)
	if err != nil {
		logger.Debug("failed to render template", "template", s, "error", err)
		return s
	}
	return rendered
}

// renderFieldTemplate renders one of a row field's templates, reporting
// false when s names a variable data lacks, such as a field declared
// later or not at all, or a setting the definition does not declare:
// Jackett fails the field then, so a required one drops its row and an
// optional one comes back empty. A template failing for any other reason
// is left as written, as renderTemplate leaves it.
func renderFieldTemplate(s string, data templateData, logger *slog.Logger) (string, bool) {
	rendered, err := renderTemplateStrict(s, data)
	if err == nil {
		return rendered, true
	}
	if _, err := executeTemplate(s, data, false); err != nil {
		logger.Debug("failed to render template", "template", s, "error", err)
		return s, true
	}
	logger.Debug("field template names a variable never set", "template", s)
	return "", false
}

// renderTemplateStrict renders s for a request that must not go out
// half-built: a template naming a key its map lacks fails, as a variable
// Jackett lacks fails its request, rather than sending "<no value>".
func renderTemplateStrict(s string, data templateData) (string, error) {
	return executeTemplate(s, data, true)
}

// executeTemplate parses and runs s against data, returning s itself when
// it holds no template. Strict, a key a map lacks is an error, whether it
// is reached by name or through a rewritten hyphenated key.
func executeTemplate(s string, data templateData, isStrict bool) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	tmpl, err := parseTemplate(s, isStrict)
	if err != nil {
		return "", err
	}
	return runTemplate(tmpl, data)
}

// parseTemplate parses s as a Cardigann template, lenient or strict about
// a key a map lacks.
func parseTemplate(s string, isStrict bool) (*template.Template, error) {
	funcs, missingKey := lenientFuncs, "missingkey=default"
	if isStrict {
		funcs, missingKey = strictFuncs, "missingkey=error"
	}
	tmpl, err := template.New("field").
		Funcs(funcs).
		Option(missingKey).
		Parse(rewriteCardigannTemplate(s))
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}
	return tmpl, nil
}

// runTemplate executes tmpl against data.
func runTemplate(tmpl *template.Template, data templateData) (string, error) {
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}

// renderSearchPath renders a search path as Jackett renders one: what each
// action writes is URL-encoded (pathValue), as a keyword placed in a path
// must be, while the path's own text, its slashes and any "?" included, is
// kept as written. Every "+" in the result then becomes "%20", the space
// Jackett's encoder writes as "+", literal text included.
func renderSearchPath(s string, data templateData) (string, error) {
	rendered := s
	if strings.Contains(s, "{{") {
		tmpl, err := parseTemplate(s, false)
		if err != nil {
			return "", err
		}
		encodeActions(tmpl.Root)
		if rendered, err = runTemplate(tmpl, data); err != nil {
			return "", err
		}
	}
	return strings.ReplaceAll(rendered, "+", "%20"), nil
}

// encodeActions ends the pipeline of every action under node that writes
// output with pathValue, so what it writes is encoded and the text between
// actions is not. An action that only declares a variable writes nothing.
func encodeActions(node parse.Node) {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, child := range n.Nodes {
			encodeActions(child)
		}
	case *parse.ActionNode:
		if len(n.Pipe.Decl) == 0 {
			n.Pipe.Cmds = append(n.Pipe.Cmds, &parse.CommandNode{
				NodeType: parse.NodeCommand,
				Pos:      n.Pos,
				Args:     []parse.Node{parse.NewIdentifier("pathValue").SetPos(n.Pos)},
			})
		}
	case *parse.IfNode:
		encodeActions(n.List)
		encodeActions(n.ElseList)
	case *parse.RangeNode:
		encodeActions(n.List)
		encodeActions(n.ElseList)
	case *parse.WithNode:
		encodeActions(n.List)
		encodeActions(n.ElseList)
	}
}

// pathValue encodes what a search path's action writes as .NET's
// WebUtility.UrlEncode does, which is Jackett's encoder: letters, digits
// and "-_.!*()" stand as they are, a space becomes "+", and every other
// byte of the UTF-8 text is percent-encoded in upper case. A value the
// template has no entry for writes nothing, as in Jackett.
func pathValue(value any) string {
	if value == nil {
		return ""
	}
	var out strings.Builder
	for _, b := range []byte(fmt.Sprint(value)) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', strings.IndexByte("-_.!*()", b) >= 0:
			out.WriteByte(b)
		case b == ' ':
			out.WriteByte('+')
		default:
			fmt.Fprintf(&out, "%%%02X", b)
		}
	}
	return out.String()
}

// applyFilters runs a Cardigann-style filter chain over a value, in order.
// Only a practical subset of Cardigann filters is supported (see
// applyFilter); an unsupported filter name is skipped and logged rather
// than applied incorrectly. Filter arguments are rendered as templates
// first, so a filter's args may reference ".Config" or ".Result".
func applyFilters(value string, filters []Filter, data templateData, logger *slog.Logger) string {
	for _, f := range filters {
		next, err := applyFilter(value, f, data, logger)
		if err != nil {
			logger.Debug("skipping filter", "filter", f.Name, "error", err)
			continue
		}
		value = next
	}
	return value
}

// applyFieldFilters is applyFilters for a row's field, reporting false,
// as renderFieldTemplate does, when a filter's argument names a variable
// data lacks: Jackett fails the field then rather than filter with what
// is missing.
func applyFieldFilters(value string, filters []Filter, data templateData, logger *slog.Logger) (string, bool) {
	for _, f := range filters {
		raw := filterArgsAsStrings(f.Args)
		args := make([]string, len(raw))
		for i, arg := range raw {
			rendered, isResolved := renderFieldTemplate(arg, data, logger)
			if !isResolved {
				return "", false
			}
			args[i] = rendered
		}
		next, err := applyFilterArgs(value, f, args, data, logger)
		if err != nil {
			logger.Debug("skipping filter", "filter", f.Name, "error", err)
			continue
		}
		value = next
	}
	return value, true
}

// filterEnv is what a filter needs besides its value and arguments: the
// definition's declared character set, which the URL filters encode to and
// from, and a logger for the two filters whose only effect is output.
type filterEnv struct {
	encoding string
	logger   *slog.Logger
}

// filterFunc applies one Cardigann filter to a value. Arguments have
// already been template-rendered and arity-checked by applyFilter.
type filterFunc func(value string, args []string, env filterEnv) (string, error)

// filterSpec is one filter's implementation and the number of arguments it
// accepts. Declaring arity here keeps the check in one place instead of
// repeating it inside every filter.
type filterSpec struct {
	apply   filterFunc
	maxArgs int // -1 for no upper bound
	minArgs int
}

// filterRegistry holds every filter Jacklet implements, keyed by the name
// a definition uses. A name absent from it is reported as unsupported
// rather than silently skipped.
var filterRegistry = map[string]filterSpec{
	"append":     {apply: filterAppend, maxArgs: 1, minArgs: 1},
	"dateparse":  {apply: filterDateParse, maxArgs: -1},
	"diacritics": {apply: filterDiacritics, maxArgs: -1, minArgs: 1},
	"fuzzytime":  {apply: filterFuzzyTime, maxArgs: -1},
	"hexdump":    {apply: filterHexDump, maxArgs: -1},
	"htmldecode": {apply: filterHTMLDecode, maxArgs: -1},
	"htmlencode": {apply: filterHTMLEncode, maxArgs: -1},
	// Cardigann's JSON path filter; "$." is optional, as in Jackett.
	"jsonjoinarray": {apply: filterJSONJoinArray, maxArgs: 2, minArgs: 2},
	"prepend":       {apply: filterPrepend, maxArgs: 1, minArgs: 1},
	"querystring":   {apply: filterQueryString, maxArgs: 1, minArgs: 1},
	"re_replace":    {apply: filterReReplace, maxArgs: 2, minArgs: 2},
	"regexp":        {apply: filterRegexp, maxArgs: 1, minArgs: 1},
	// "reltime" is Jackett's alias for "timeago".
	"reltime": {apply: filterTimeAgo, maxArgs: -1},
	"replace": {apply: filterReplace, maxArgs: 2, minArgs: 2},
	"reverse": {apply: filterReverse, maxArgs: -1},
	"split":   {apply: filterSplit, maxArgs: 2, minArgs: 2},
	"strdump": {apply: filterStrDump, maxArgs: -1},
	"timeago": {apply: filterTimeAgo, maxArgs: -1},
	// Jackett implements "timeparse" and "dateparse" as the same case.
	"timeparse": {apply: filterDateParse, maxArgs: -1},
	// Cardigann spells the case filters both ways, and real definitions
	// use both.
	"tolower":       {apply: filterToLower, maxArgs: -1},
	"tolowercase":   {apply: filterToLower, maxArgs: -1},
	"toupper":       {apply: filterToUpper, maxArgs: -1},
	"touppercase":   {apply: filterToUpper, maxArgs: -1},
	"trim":          {apply: filterTrim, maxArgs: -1},
	"urldecode":     {apply: filterURLDecode, maxArgs: -1},
	"urlencode":     {apply: filterURLEncode, maxArgs: -1},
	"validate":      {apply: filterValidate, maxArgs: -1, minArgs: 1},
	"validfilename": {apply: filterValidFilename, maxArgs: -1},
}

// checkArgs reports whether args satisfies the filter's declared arity.
func (spec filterSpec) checkArgs(name string, args []string) error {
	if len(args) < spec.minArgs {
		return fmt.Errorf("%s: expected %d args, got %d", name, spec.minArgs, len(args))
	}
	if spec.maxArgs >= 0 && len(args) > spec.maxArgs {
		return fmt.Errorf("%s: expected at most %d args, got %d", name, spec.maxArgs, len(args))
	}
	return nil
}

func applyFilter(value string, f Filter, data templateData, logger *slog.Logger) (string, error) {
	return applyFilterArgs(value, f, renderArgs(filterArgsAsStrings(f.Args), data, logger), data, logger)
}

// applyFilterArgs is applyFilter with the arguments already rendered.
func applyFilterArgs(value string, f Filter, args []string, data templateData, logger *slog.Logger) (string, error) {
	spec, ok := filterRegistry[f.Name]
	if !ok {
		return value, fmt.Errorf("unsupported filter %q", f.Name)
	}

	if err := spec.checkArgs(f.Name, args); err != nil {
		return value, err
	}
	return spec.apply(value, args, filterEnv{encoding: data.encoding, logger: logger})
}

// dotNetReplacementRe matches one substitution token in a .NET regex
// replacement string.
var dotNetReplacementRe = regexp.MustCompile(`\$(\$|&|\{[A-Za-z0-9_]+\}|\d+|[A-Za-z_][A-Za-z0-9_]*)`)

// goReplacement rewrites a .NET regex replacement string as Go's
// equivalent. The two agree on "$1" alone but diverge the moment a
// reference is followed by text: .NET reads "$1x" as group 1 then a
// literal "x", while Go reads it as a group named "1x" and substitutes
// nothing. Braces make the boundary explicit and mean the same in both.
// "$&" is .NET's whole-match reference, which Go spells "${0}".
func goReplacement(replacement string) string {
	return dotNetReplacementRe.ReplaceAllStringFunc(replacement, func(token string) string {
		switch body := token[1:]; {
		case body == "$":
			return "$$"
		case body == "&":
			return "${0}"
		case strings.HasPrefix(body, "{"):
			return "${" + strings.Trim(body, "{}") + "}"
		default:
			return "${" + body + "}"
		}
	})
}

func filterReReplace(value string, args []string, env filterEnv) (string, error) {
	re, err := regexp.Compile(args[0])
	if err != nil {
		return value, err
	}
	return re.ReplaceAllString(value, goReplacement(args[1])), nil
}

func filterQueryString(value string, args []string, env filterEnv) (string, error) {
	return extractQueryParam(value, args[0])
}

// filterTrim is Cardigann's "trim". Jackett calls String.Trim(cutset[0]),
// so only the first character of the argument is ever trimmed; a
// definition passing "ab" trims 'a' alone. Matching that quirk matters
// because definitions are written and tested against Jackett.
func filterTrim(value string, args []string, env filterEnv) (string, error) {
	if len(args) == 0 || args[0] == "" {
		return strings.TrimSpace(value), nil
	}
	cutset, _ := utf8.DecodeRuneInString(args[0])
	return strings.Trim(value, string(cutset)), nil
}

func filterToLower(value string, _ []string, env filterEnv) (string, error) {
	return strings.ToLower(value), nil
}
func filterToUpper(value string, _ []string, env filterEnv) (string, error) {
	return strings.ToUpper(value), nil
}

// invalidFilenameRune matches what .NET's Path.GetInvalidFileNameChars
// reports, which is what Jackett's "validfilename" filter replaces: the
// control characters plus the separators and wildcards Windows reserves.
// The set is fixed rather than taken from the host, so a definition
// yields the same value here as it does in Jackett on any platform.
func invalidFilenameRune(r rune) bool {
	return r < 0x20 || strings.ContainsRune(`"<>|:*?\/`, r)
}

// filterValidFilename is Cardigann's "validfilename". Jackett calls
// MakeValidFileName with '_' and without its "fancy" substitutions, so
// every invalid character becomes an underscore rather than a lookalike,
// and a value left empty becomes "_".
func filterValidFilename(value string, _ []string, env filterEnv) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if invalidFilenameRune(r) {
			return '_'
		}
		return r
	}, value)
	if cleaned == "" {
		return "_", nil
	}
	return cleaned, nil
}

func filterAppend(value string, args []string, env filterEnv) (string, error) {
	return value + args[0], nil
}
func filterPrepend(value string, args []string, env filterEnv) (string, error) {
	return args[0] + value, nil
}

// filterSplit is Cardigann's "split". Like Jackett's String.Split(sep[0])
// it separates on the first character of the argument only, and a
// negative index counts back from the end.
func filterSplit(value string, args []string, env filterEnv) (string, error) {
	idx, err := strconv.Atoi(args[1])
	if err != nil {
		return value, err
	}
	if args[0] == "" {
		return value, errors.New("split: empty separator")
	}
	separator, _ := utf8.DecodeRuneInString(args[0])
	parts := strings.Split(value, string(separator))
	if idx < 0 {
		idx += len(parts)
	}
	if idx < 0 || idx >= len(parts) {
		return value, fmt.Errorf("split: index %d out of range (%d parts)", idx, len(parts))
	}
	return parts[idx], nil
}

// dotNetURLSafe reports whether a byte is left unescaped by .NET's
// WebUtility.UrlEncode. It differs from Go's url.QueryEscape at five
// characters: .NET keeps "!*()" literal and escapes "~".
func dotNetURLSafe(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '!', c == '*', c == '(', c == ')':
		return true
	default:
		return false
	}
}

// filterURLEncode percent-encodes a value the way Jackett does: through
// the definition's character set, so a windows-1251 tracker receives
// cp1251 bytes rather than UTF-8 ones, and using .NET's unreserved set.
func filterURLEncode(value string, _ []string, env filterEnv) (string, error) {
	raw, err := encodeBytes(value, env.encoding)
	if err != nil {
		return value, err
	}
	return percentEncodeWith(raw, dotNetURLSafe), nil
}

// filterURLDecode is filterURLEncode's inverse, reading the decoded bytes
// in the definition's character set.
func filterURLDecode(value string, _ []string, env filterEnv) (string, error) {
	raw, err := percentDecode(value)
	if err != nil {
		return value, err
	}
	decoded, err := decodeBytes(raw, env.encoding)
	if err != nil {
		return value, err
	}
	return decoded, nil
}

// normalizeSpace collapses runs of whitespace, as Jackett's
// ParseUtil.NormalizeSpace does before every date parse.
func normalizeSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// filterDateParse backs both "dateparse" and "timeparse", which Jackett
// implements as one case. The layout may be written either as a .NET
// format or as a Go layout; see parseDeclaredLayout.
func filterDateParse(value string, args []string, env filterEnv) (string, error) {
	value = normalizeSpace(value)

	// Cardigann requires a layout here. Without one there is nothing to
	// parse against, so fall back to the heuristics rather than failing.
	if len(args) == 0 || args[0] == "" {
		return parseUnknownTime(value)
	}

	parsed, err := parseDeclaredLayout(value, args[0])
	if err != nil {
		// Jackett logs and leaves the value untouched rather than guessing
		// at a value the definition already described. The date field is
		// resolved heuristically later anyway (see torrentFrom), so a
		// mismatched layout still yields a date -- it just does not put a
		// guess into a value a later filter in the chain would read.
		return value, err
	}
	return parsed.Format(time.RFC3339), nil
}

// filterTimeAgo backs "timeago" and its "reltime" alias, which resolve
// relative text only.
func filterTimeAgo(value string, _ []string, env filterEnv) (string, error) {
	return parseTimeAgo(normalizeSpace(value))
}

// filterFuzzyTime backs "fuzzytime", Jackett's FromUnknown: whatever the
// tracker prints, in whatever shape.
func filterFuzzyTime(value string, _ []string, env filterEnv) (string, error) {
	return parseUnknownTime(value)
}

// isAllDigits reports whether every character is an ASCII digit, the test
// Jackett uses to recognize a bare Unix timestamp.
func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseUnknownTime resolves a timestamp of unknown shape, following the
// order Jackett's FromUnknown tries: its own RFC1123Z output first (so a
// value that has already been through a date filter round-trips), then a
// bare Unix timestamp, then relative text, then absolute parsing.
func parseUnknownTime(value string) (string, error) {
	t, err := parseUnknownTimeValue(value)
	if err != nil {
		return value, err
	}
	return t.Format(time.RFC3339), nil
}

// parseUnknownTimeValue is parseUnknownTime returning the instant itself,
// for callers that store a time rather than a rendered string. It follows
// the order Jackett's FromUnknown tries, since an earlier shape would
// otherwise be misread by a later one.
func parseUnknownTimeValue(value string) (time.Time, error) {
	value = normalizeSpace(value)
	now := time.Now()

	// Jacklet's own rendering first, so a value that has already been
	// through a date filter round-trips.
	if parsed, err := time.Parse(time.RFC1123Z, value); err == nil {
		return parsed, nil
	}
	if isAllDigits(value) {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			return time.Unix(seconds, 0), nil
		}
	}
	if strings.Contains(strings.ToLower(value), "now") {
		return now, nil
	}
	// Gated on the word, as Jackett gates it: without that, a date merely
	// holding a number and a word reads as "N units ago".
	if agoRe.MatchString(value) {
		if relative, err := parseTimeAgoValue(value); err == nil {
			return relative, nil
		}
	}

	for _, relative := range []struct {
		expr   *regexp.Regexp
		offset int
	}{
		{expr: todayRe, offset: 0},
		{expr: yesterdayRe, offset: -1},
		{expr: tomorrowRe, offset: 1},
	} {
		if parsed, ok := relativeDay(value, relative.expr, relative.offset, now); ok {
			return parsed, nil
		}
	}
	if parsed, ok := weekdayBefore(value, now); ok {
		return parsed, nil
	}

	// A date whose year the tracker left out: supply this one, and step
	// back when that puts it in the future.
	if matched := missingYearRe.FindStringSubmatch(value); matched != nil {
		dated := strings.Replace(value, matched[1], strconv.Itoa(now.Year())+"-"+matched[1], 1)
		if parsed, err := dateparse.ParseIn(dated, time.Local); err == nil { //nolint:gosmopolitan // a tracker's zone-less timestamp is its own wall clock; see parseDeclaredLayout
			return notInTheFuture(parsed, now), nil
		}
	}
	if matched := missingYearDayMonthRe.FindStringSubmatch(value); matched != nil {
		dated := matched[1] + " " + strconv.Itoa(now.Year()) + " " + matched[2]
		if parsed, err := dateparse.ParseIn(dated, time.Local); err == nil { //nolint:gosmopolitan // as above
			return notInTheFuture(parsed, now), nil
		}
	}

	// A bare time of day is today's, which is how a tracker lists what it
	// published in the last few hours. This has to come before the general
	// parser rather than after it: that one reads "7:05pm" as the 1st of
	// July in year zero and reports no error at all.
	if clock, ok := parseTimeOfDay(value); ok {
		return clock.at(now), nil
	}

	parsed, err := dateparse.ParseIn(value, time.Local) //nolint:gosmopolitan // as above
	if err != nil {
		return time.Time{}, err
	}
	// Year zero means the parser matched a shape without ever finding a
	// date. Returning it would store a release two thousand years old.
	if parsed.Year() == 0 {
		return time.Time{}, fmt.Errorf("parseUnknownTime: %q yielded no date", value)
	}
	return parsed, nil
}

func filterReplace(value string, args []string, env filterEnv) (string, error) {
	return strings.ReplaceAll(value, args[0], args[1]), nil
}

func filterHTMLDecode(value string, _ []string, env filterEnv) (string, error) {
	return html.UnescapeString(value), nil
}

// filterHTMLEncode mirrors .NET's WebUtility.HtmlEncode, which Go's
// html.EscapeString does not: .NET spells the double quote "&quot;"
// rather than "&#34;", and numerically encodes the Latin-1 supplement
// and everything outside the BMP while leaving the rest of Unicode alone.
func filterHTMLEncode(value string, _ []string, env filterEnv) (string, error) {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		case '&':
			b.WriteString("&amp;")
		default:
			if (r >= 160 && r < 256) || r > 0xFFFF {
				fmt.Fprintf(&b, "&#%d;", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String(), nil
}

// filterRegexp returns the first capture group of the first match, which
// is how definitions pull one substring out of a larger blob of text.
//
// Jackett reads Match.Groups[1].Value unconditionally, which is "" when
// the pattern does not match or has no capture group. Returning the
// untouched input instead would be worse than returning nothing: a failed
// extraction would silently pass the whole source blob through as the
// field's value.
func filterRegexp(value string, args []string, env filterEnv) (string, error) {
	re, err := regexp.Compile(args[0])
	if err != nil {
		return value, err
	}
	m := re.FindStringSubmatch(value)
	if len(m) > 1 {
		return m[1], nil
	}
	return "", nil
}

// validateDelimiters is the set Jackett's "validate" filter tokenizes on,
// for both its argument and the value under test.
const validateDelimiters = `, /().;[]"|:`

// validateTokens lowercases and splits on validateDelimiters, dropping
// empty entries the way .NET's StringSplitOptions.RemoveEmptyEntries does.
func validateTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return strings.ContainsRune(validateDelimiters, r)
	})
}

// filterValidate keeps only the accepted tokens that the value actually
// contains, so a definition can strip unexpected markup down to the terms
// it recognizes. Jackett implements it as a set intersection joined with
// commas -- not as an all-or-nothing check -- and an empty result is a
// normal outcome rather than an error.
func filterValidate(value string, args []string, env filterEnv) (string, error) {
	have := make(map[string]bool)
	for _, tok := range validateTokens(value) {
		have[tok] = true
	}

	// .NET's Intersect yields the first sequence's order, deduplicated.
	var out []string
	emitted := make(map[string]bool)
	for _, want := range validateTokens(strings.Join(args, ",")) {
		if have[want] && !emitted[want] {
			emitted[want] = true
			out = append(out, want)
		}
	}
	return strings.Join(out, ","), nil
}

// filterDiacritics is Cardigann's "diacritics", which folds accented Latin
// text to its base letters so a query matches a title spelled either way.
//
// "replace" is the only operation Jackett implements, and it raises on
// anything else rather than guessing. An unrecognized argument is an error
// here for the same reason: folding regardless would apply a
// transformation the definition did not ask for, and every definition that
// uses this filter passes "replace".
func filterDiacritics(value string, args []string, _ filterEnv) (string, error) {
	if args[0] != "replace" {
		return value, fmt.Errorf("diacritics: unsupported argument %q", args[0])
	}
	return removeDiacritics(value), nil
}

// filterJSONJoinArray is Cardigann's "jsonjoinarray": it pulls an array
// out of a JSON document by path and joins its elements. Definitions use
// it to flatten a list the tracker returns, such as a row's tags.
func filterJSONJoinArray(value string, args []string, env filterEnv) (string, error) {
	var document any
	if err := json.Unmarshal([]byte(value), &document); err != nil {
		return value, fmt.Errorf("jsonjoinarray: %w", err)
	}
	found, ok := jsonLookup(document, args[0])
	if !ok {
		return value, fmt.Errorf("jsonjoinarray: nothing at %q", args[0])
	}
	items, ok := found.([]any)
	if !ok {
		return value, fmt.Errorf("jsonjoinarray: %q is not an array", args[0])
	}

	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = jsonScalar(item)
	}
	return strings.Join(parts, args[1]), nil
}

// filterHexDump is Cardigann's "hexdump", which renders each character
// beside its code point so that an invisible or look-alike character -- a
// non-breaking space, a zero-width joiner, a mis-decoded byte -- becomes
// visible. The value passes through untouched: the log line is the whole
// point, so a definition reaching for it must not produce nothing.
func filterHexDump(value string, _ []string, env filterEnv) (string, error) {
	var b strings.Builder
	for _, r := range value {
		fmt.Fprintf(&b, "%c(%02X)", r, r)
	}
	env.logger.Debug("hexdump", "value", b.String())
	return value, nil
}

// filterStrDump is Cardigann's "strdump": the value with the whitespace
// that does not show itself spelled out, under the optional tag a
// definition passes to tell one dump from another.
func filterStrDump(value string, args []string, env filterEnv) (string, error) {
	dumped := strDumpReplacer.Replace(value)
	if len(args) > 0 && args[0] != "" {
		env.logger.Debug("strdump", "tag", args[0], "value", dumped)
	} else {
		env.logger.Debug("strdump", "value", dumped)
	}
	return value, nil
}

// strDumpReplacer spells out what Jackett escapes in a strdump: the two
// line endings, and the non-breaking space that looks exactly like a space
// and makes a trim or a comparison quietly do nothing.
var strDumpReplacer = strings.NewReplacer("\r", `\r`, "\n", `\n`, "\u00a0", `\xA0`)

func filterReverse(value string, _ []string, env filterEnv) (string, error) {
	runes := []rune(value)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes), nil
}

// removeDiacritics strips combining marks, folding accented Latin text to
// its base letters so a query matches a title spelled either way.
func removeDiacritics(value string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(value) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(b.String())
}

// timeAgoTokenRe matches one "<number><unit>" pair inside relative-time
// text, mirroring the regex Jackett's DateTimeUtil.FromTimeAgo uses.
var timeAgoTokenRe = regexp.MustCompile(`([\d.]+)\s*?([^\d\s.]+)`)

// timeAgoReplacer strips the filler Jackett removes before matching, so
// "1 day, 2 hours ago" reduces to a run of number/unit pairs.
var timeAgoReplacer = strings.NewReplacer(",", "", "ago", "", "and", "")

// timeAgoUnit resolves a unit token to its duration. Jackett matches by
// substring, so "hours", "hour" and "hr" all mean an hour, and the order
// of the checks is load-bearing — it is Jackett's.
func timeAgoUnit(unit string) (time.Duration, bool) {
	switch {
	case strings.Contains(unit, "sec") || unit == "s":
		return time.Second, true
	case strings.Contains(unit, "min") || unit == "m":
		return time.Minute, true
	case strings.Contains(unit, "hour") || strings.Contains(unit, "hr") || unit == "h":
		return time.Hour, true
	case strings.Contains(unit, "day") || unit == "d":
		return 24 * time.Hour, true
	case strings.Contains(unit, "week") || strings.Contains(unit, "wk") || unit == "w":
		return 7 * 24 * time.Hour, true
	case strings.Contains(unit, "month") || unit == "mo":
		return 30 * 24 * time.Hour, true
	case strings.Contains(unit, "year") || unit == "y":
		return 365 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

// parseTimeAgo converts relative-time text into an absolute RFC3339
// timestamp, following Jackett's FromTimeAgo: "now" is the current time,
// abbreviated units are understood, and *every* number/unit pair in the
// text contributes, so "1 day 2 hours ago" is 26 hours rather than 2.
//
// Text that is not relative at all yields an error so the caller can fall
// back to absolute parsing and the filter chain leaves the value alone.
func parseTimeAgo(value string) (string, error) {
	t, err := parseTimeAgoValue(value)
	if err != nil {
		return value, err
	}
	return t.Format(time.RFC3339), nil
}

// parseTimeAgoValue is parseTimeAgo returning the instant itself.
func parseTimeAgoValue(value string) (time.Time, error) {
	lowered := strings.ToLower(value)
	if strings.Contains(lowered, "now") {
		return time.Now(), nil
	}

	matches := timeAgoTokenRe.FindAllStringSubmatch(timeAgoReplacer.Replace(lowered), -1)
	if len(matches) == 0 {
		return time.Time{}, fmt.Errorf("timeago: no relative time found in %q", value)
	}

	var total time.Duration
	for _, m := range matches {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return time.Time{}, err
		}
		unit, ok := timeAgoUnit(m[2])
		if !ok {
			return time.Time{}, fmt.Errorf("timeago: unknown unit %q", m[2])
		}
		total += time.Duration(n * float64(unit))
	}

	return time.Now().Add(-total), nil
}

func renderArgs(args []string, data templateData, logger *slog.Logger) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = renderTemplate(a, data, logger)
	}
	return out
}

func extractQueryParam(raw string, param string) (string, error) {
	if u, err := url.Parse(raw); err == nil && u.RawQuery != "" {
		if v := u.Query().Get(param); v != "" {
			return v, nil
		}
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return raw, err
	}
	return values.Get(param), nil
}

// filterArgsAsStrings normalizes a filter's YAML-decoded args (a bare
// scalar, or a list of scalars) into a string slice.
func filterArgsAsStrings(args any) []string {
	switch v := args.(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprintf("%v", item))
		}
		return out
	default:
		return []string{fmt.Sprintf("%v", v)}
	}
}
