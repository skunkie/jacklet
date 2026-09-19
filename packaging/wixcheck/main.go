// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Command wixcheck holds the installer authoring to the rules that
// nothing else enforces.
//
// Each check here exists because the mistake it catches reached a build,
// a release or a machine before anything noticed. They are the ones that
// produce authoring which is accepted, builds, installs, and quietly does
// not do what it says: an assignment that can never fire, a dialog that
// was never linked, a box that renders ticked while the thing it offers
// is switched off. The toolset reports none of them, because none of them
// is malformed.
//
// It reads the source rather than a built package, so it needs no Windows
// and runs wherever the rest of the checks do.
package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// authoring is as much of the WiX schema as these checks need. Everything
// is captured as a nested element so that one pass over a file can reach
// components, properties and controls wherever they are declared.
type authoring struct {
	Components []component
	Controls   []control
	Properties []property
	RegKeys    []registryKey
	SetProps   []setProp
	UIRefs     []uiElement
	UIs        []uiElement
}

type component struct {
	Files   int
	Guid    string
	ID      string
	IsKeyed bool
}

// registryKey is a RegistryKey element, which is the only authoring that
// can give a key up; a RegistryValue on its own names a key it does not own.
type registryKey struct {
	Component string
	IsGivenUp bool
	Key       string
}

// Hidden is named for the WiX attribute it carries rather than as a
// predicate, because the diagnostic quotes the attribute back at whoever
// has to go and add it.
type property struct {
	Hidden bool
	ID     string
	Value  string
}

// NoPrefix carries the WiX attribute of that name, and reads as one for
// the same reason property.Hidden does.
type control struct {
	ID       string
	NoPrefix bool
	Property string
	Text     string
	Type     string
}

type setProp struct {
	Action    string
	Condition string
	ID        string
}

type uiElement struct {
	ID string
}

// emptinessGuard matches a condition that only holds while a property has
// no value of its own.
var emptinessGuard = regexp.MustCompile(`^(\w+)=""`)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: wixcheck <file.wxs>...\n       wixcheck -ipl <file.wixipl>")
		os.Exit(2)
	}

	// A built intermediate holds this project's authoring and the
	// toolset's dialog library in one table, which is the only place some
	// of these can be seen.
	if os.Args[1] == "-ipl" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: wixcheck -ipl <file.wixipl>")
			os.Exit(2)
		}
		problems, err := checkIntermediate(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		report(problems)
		fmt.Println("wixcheck: every wizard navigation is reachable.")
		return
	}

	var all authoring
	for _, path := range os.Args[1:] {
		if err := read(path, &all); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	report(check(&all))
	fmt.Printf("wixcheck: %d components, %d properties, %d assignments; nothing unreachable.\n",
		len(all.Components), len(all.Properties), len(all.SetProps))
}

// report prints anything found and stops, since none of it is advisory.
func report(problems []string) {
	if len(problems) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "installer authoring problems:")
	for _, problem := range problems {
		fmt.Fprintf(os.Stderr, "  %s\n", problem)
	}
	os.Exit(1)
}

// read parses one authoring file into into.
//
// The paths are this command's own arguments, given by a developer or by
// the workflow, naming files in this repository.
func read(path string, into *authoring) error {
	data, err := os.ReadFile(path) //nolint:gosec // a path the caller chose, not one from input.
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := collect(data, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// collect walks the document, gathering the elements the checks need.
//
// A hand-rolled walk rather than struct tags, because these elements
// appear at many depths and under several parents, and a shape that
// described all of them would have to describe the whole schema.
func collect(data []byte, into *authoring) error {
	// encoding/xml resolves no external entities, so there is nothing here
	// for a document to reach outside itself with.
	decoder := xml.NewDecoder(bytes.NewReader(data)) //nolint:gosec // no entity resolution to abuse.

	// The component currently open, so its files and keypath can be
	// counted as they are passed.
	var open *component

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parsing: %w", err)
		}

		switch element := token.(type) {
		case xml.StartElement:
			attrs := attributes(element)
			open = record(element.Name.Local, attrs, into, open)
			if open != nil && attrs["KeyPath"] == "yes" {
				open.IsKeyed = true
			}
		case xml.EndElement:
			if element.Name.Local == "Component" {
				open = nil
			}
		}
	}
}

// record files one element into into, returning the component left open:
// the one a File or a KeyPath that follows belongs to.
func record(element string, attrs map[string]string, into *authoring, open *component) *component {
	switch element {
	case "Component":
		into.Components = append(into.Components, component{Guid: attrs["Guid"], ID: attrs["Id"]})
		open = &into.Components[len(into.Components)-1]
	case "File":
		if open != nil {
			open.Files++
		}
	case "CreateFolder":
		if open != nil {
			open.IsKeyed = true
		}
	case "RegistryKey":
		name := ""
		if open != nil {
			name = open.ID
		}
		into.RegKeys = append(into.RegKeys, registryKey{
			Component: name,
			IsGivenUp: attrs["ForceDeleteOnUninstall"] == "yes",
			Key:       attrs["Key"],
		})
	case "Property":
		into.Properties = append(into.Properties, property{
			Hidden: attrs["Hidden"] == "yes",
			ID:     attrs["Id"],
			Value:  attrs["Value"],
		})
	case "Control":
		into.Controls = append(into.Controls, control{
			ID:       attrs["Id"],
			NoPrefix: attrs["NoPrefix"] == "yes",
			Property: attrs["Property"],
			Text:     attrs["Text"],
			Type:     attrs["Type"],
		})
	case "SetProperty":
		into.SetProps = append(into.SetProps, setProp{
			Action: attrs["Action"], Condition: attrs["Condition"], ID: attrs["Id"],
		})
	case "UI":
		if attrs["Id"] != "" {
			into.UIs = append(into.UIs, uiElement{ID: attrs["Id"]})
		}
	case "UIRef":
		into.UIRefs = append(into.UIRefs, uiElement{ID: attrs["Id"]})
	}
	return open
}

func attributes(element xml.StartElement) map[string]string {
	attrs := make(map[string]string, len(element.Attr))
	for _, attr := range element.Attr {
		attrs[attr.Name.Local] = attr.Value
	}
	return attrs
}

func check(all *authoring) []string {
	defaults := make(map[string]string, len(all.Properties))
	for _, prop := range all.Properties {
		if prop.Value != "" {
			defaults[prop.ID] = prop.Value
		}
	}

	problems := make([]string, 0, len(all.Components))
	problems = append(problems, checkComponents(all)...)
	problems = append(problems, checkRegistryKeys(all)...)
	problems = append(problems, checkAssignments(all, defaults)...)
	problems = append(problems, checkCheckBoxes(all, defaults)...)
	problems = append(problems, checkAccelerators(all)...)
	problems = append(problems, checkReferences(all)...)
	problems = append(problems, checkCredentials(all)...)
	return problems
}

// checkComponents reports a component Windows Installer cannot track: one
// with no key path, or one whose guid it would have to generate from a key
// path that several files share.
func checkComponents(all *authoring) []string {
	var problems []string
	for _, comp := range all.Components {
		// A generated guid comes from the keypath, so a component holding
		// several unversioned files has nothing unique to derive it from.
		if comp.Guid == "" && comp.Files > 1 {
			problems = append(problems, fmt.Sprintf(
				"component %s has %d files and no explicit Guid; give it one component per file, or a Guid",
				comp.ID, comp.Files))
		}
		if !comp.IsKeyed {
			problems = append(problems, fmt.Sprintf(
				"component %s has no KeyPath and creates no folder", comp.ID))
		}
	}

	return problems
}

// checkRegistryKeys reports a key the installer creates and never gives up.
//
// Windows Installer removes the values a component wrote and leaves the key
// that held them, so a key outlives the product unless ForceDeleteOnUninstall
// says otherwise. What it leaves behind is a machine an operator uninstalled
// from that still carries the product's configuration, credentials included.
func checkRegistryKeys(all *authoring) []string {
	var problems []string
	for _, key := range all.RegKeys {
		if key.IsGivenUp {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"component %s creates %s but does not set ForceDeleteOnUninstall, so the key survives the uninstall",
			key.Component, key.Key))
	}

	return problems
}

// checkAssignments reports a SetProperty that can never fire.
//
// An assignment guarded on a property being empty can only fire if that
// property does not arrive with a value. One that does is not a weaker
// guard, it is an assignment that never happens.
func checkAssignments(all *authoring, defaults map[string]string) []string {
	var problems []string
	for _, set := range all.SetProps {
		guard := emptinessGuard.FindStringSubmatch(set.Condition)
		if guard == nil {
			continue
		}
		if value, ok := defaults[guard[1]]; ok {
			name := set.Action
			if name == "" {
				name = set.ID
			}
			problems = append(problems, fmt.Sprintf(
				"assignment %s is guarded on %s being empty, but that property defaults to %q, so it never fires",
				name, guard[1], value))
		}
	}

	return problems
}

// checkCheckBoxes reports a box that renders ticked regardless of what it
// means.
//
// A check box renders ticked whenever its property has any value, so one
// with a default offers its option as already chosen while the component
// that acts on it, which tests for a particular value, does nothing.
func checkCheckBoxes(all *authoring, defaults map[string]string) []string {
	var problems []string
	for _, ctrl := range all.Controls {
		if ctrl.Type != "CheckBox" || ctrl.Property == "" {
			continue
		}
		if value, ok := defaults[ctrl.Property]; ok {
			problems = append(problems, fmt.Sprintf(
				"check box %s is bound to %s, which defaults to %q, so the box renders ticked whatever it means",
				ctrl.ID, ctrl.Property, value))
		}
	}

	return problems
}

// checkAccelerators reports a label that asks for a keyboard accelerator
// and suppresses the markup that would draw it.
//
// An ampersand in a control's text marks the letter after it as the
// accelerator, and NoPrefix is what turns that off. A label carrying both
// gets neither: the underline it asked for, nor a text the reader can make
// sense of, since the ampersand is then drawn as itself.
func checkAccelerators(all *authoring) []string {
	var problems []string
	for _, ctrl := range all.Controls {
		if !ctrl.NoPrefix || !strings.Contains(ctrl.Text, "&") {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"control %s sets NoPrefix and its text holds an ampersand, which is drawn as one: %q",
			ctrl.ID, ctrl.Text))
	}

	return problems
}

// checkReferences reports a UI fragment nothing links in.
//
// A fragment is linked only if something refers to it, and losing one is
// silent: the stock dialogs run in its place, and an install that supplies
// every value on the command line cannot tell.
func checkReferences(all *authoring) []string {
	var problems []string
	referenced := make(map[string]bool, len(all.UIRefs))
	for _, ref := range all.UIRefs {
		referenced[ref.ID] = true
	}
	for _, ui := range all.UIs {
		if !referenced[ui.ID] {
			problems = append(problems, fmt.Sprintf(
				"UI %s is never referenced by a UIRef, so its fragment is dropped without a word", ui.ID))
		}
	}

	return problems
}

// checkCredentials reports a credential property that an install log would
// record in full, which is any that is not Hidden.
func checkCredentials(all *authoring) []string {
	var problems []string
	for _, prop := range all.Properties {
		if !looksLikeCredential(prop.ID) {
			continue
		}
		if !hidden(all, prop.ID) {
			problems = append(problems, fmt.Sprintf(
				"property %s looks like a credential but is not Hidden", prop.ID))
		}
	}

	return problems
}

// credentialNames are the words that mark a property as holding something
// that should not be written to a log.
var credentialNames = []string{"APIKEY", "PASSWORD", "SECRET", "TOKEN"}

// looksLikeCredential reports whether id names a property that carries a
// credential.
//
// A property holding the result of hashing one is not the thing itself,
// and is stored precisely so the credential need not be.
func looksLikeCredential(id string) bool {
	if strings.Contains(id, "HASH") || strings.HasPrefix(id, "EXISTING") {
		return false
	}
	for _, name := range credentialNames {
		if strings.Contains(id, name) {
			return true
		}
	}
	return false
}

// hidden reports whether any declaration of the property marks it Hidden.
func hidden(all *authoring, id string) bool {
	for _, prop := range all.Properties {
		if prop.ID == id && prop.Hidden {
			return true
		}
	}
	return false
}
