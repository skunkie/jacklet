// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// dialogMoves are the control events that take the wizard somewhere else.
// The installer publishes at most one of them per control.
var dialogMoves = map[string]bool{"NewDialog": true, "SpawnDialog": true}

// controlEvent is one row of the table that drives the wizard.
type controlEvent struct {
	Argument  string
	Condition string
	Control   string
	Dialog    string
	Event     string
	Ordering  int
	Source    string
}

// checkIntermediate reads a built intermediate and reports what only the
// whole package can show.
//
// The authoring this command otherwise reads is one half of the wizard.
// The other half comes from the toolset's own dialog library, and the two
// are only in the same table once a package has been linked. A navigation
// that is correct on its own can still be overruled by a row this project
// does not contain, which is exactly what happened: a move to the settings
// page published below the library's move to the confirmation, and so
// never happened at all.
func checkIntermediate(path string) ([]string, error) {
	events, err := readControlEvents(path)
	if err != nil {
		return nil, err
	}
	return reachability(events), nil
}

// reachability reports every dialog move that cannot be the one published.
func reachability(events []controlEvent) []string {
	// Grouped by the control that publishes them, which is the scope the
	// installer picks a single move from.
	byControl := make(map[string][]controlEvent)
	for _, event := range events {
		if !dialogMoves[event.Event] {
			continue
		}
		key := event.Dialog + "/" + event.Control
		byControl[key] = append(byControl[key], event)
	}

	var problems []string
	for key, moves := range byControl {
		sort.Slice(moves, func(i, j int) bool { return moves[i].Ordering < moves[j].Ordering })

		// A move with no condition always applies, so nothing ordered
		// below it can ever be the one published.
		for i, move := range moves {
			if move.Condition != "" {
				continue
			}
			for _, lower := range moves[:i] {
				if lower.Condition == "" {
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"%s publishes %s to %s at %d, but %s to %s at %d has no condition and a larger ordering, "+
						"so only that one is ever published\n      (%s)",
					key, lower.Event, lower.Argument, lower.Ordering,
					move.Event, move.Argument, move.Ordering, lower.Source))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// readControlEvents pulls the control event rows out of an intermediate,
// which is a zip holding the resolved symbols as JSON.
func readControlEvents(path string) ([]controlEvent, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer archive.Close()

	var raw []byte
	for _, file := range archive.File {
		if file.Name != "wix-ir.json" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		raw, err = io.ReadAll(reader)
		reader.Close()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	}
	if raw == nil {
		return nil, fmt.Errorf("%s holds no wix-ir.json; is it an intermediate?", path)
	}

	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var events []controlEvent
	walkSymbols(document, "ControlEvent", func(symbol map[string]any) {
		fields := symbolFields(symbol)
		if len(fields) < 6 {
			return
		}
		order, _ := fields[5].(float64)
		events = append(events, controlEvent{
			Argument:  text(fields[3]),
			Condition: text(fields[4]),
			Control:   text(fields[1]),
			Dialog:    text(fields[0]),
			Event:     text(fields[2]),
			Ordering:  int(order),
			Source:    symbolSource(symbol),
		})
	})
	return events, nil
}

// walkSymbols visits every symbol of the named type.
func walkSymbols(node any, want string, visit func(map[string]any)) {
	switch value := node.(type) {
	case map[string]any:
		if kind, _ := value["type"].(string); kind == want {
			visit(value)
		}
		for _, child := range value {
			walkSymbols(child, want, visit)
		}
	case []any:
		for _, child := range value {
			walkSymbols(child, want, visit)
		}
	}
}

// symbolFields returns a symbol's columns, each of which is either null or
// an object holding the value.
func symbolFields(symbol map[string]any) []any {
	raw, _ := symbol["fields"].([]any)
	fields := make([]any, len(raw))
	for i, cell := range raw {
		if held, ok := cell.(map[string]any); ok {
			fields[i] = held["data"]
		}
	}
	return fields
}

// symbolSource names the file a symbol came from, which is what tells a
// row this project wrote from one the toolset's library did.
func symbolSource(symbol map[string]any) string {
	location, _ := symbol["ln"].(map[string]any)
	file, _ := location["file"].(string)
	line, _ := location["line"].(float64)
	if file == "" {
		return "unknown"
	}
	for i := len(file) - 1; i >= 0; i-- {
		if file[i] == '/' || file[i] == '\\' {
			file = file[i+1:]
			break
		}
	}
	return fmt.Sprintf("%s:%d", file, int(line))
}

func text(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}
