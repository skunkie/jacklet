// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCloneTracker fills every field of a Tracker, nested ones included,
// and fails on any slice, map or pointer its copy still shares with it, so
// a field added to a definition is caught until cloneTracker copies it.
func TestCloneTracker(t *testing.T) {
	var source Tracker
	fillDefinition(t, reflect.ValueOf(&source).Elem(), "Tracker")

	cloned := cloneTracker(&source)
	require.Equal(t, source, cloned, "the copy differs from the definition it was taken from")

	sourceReferences := make(map[uintptr]string)
	collectReferences(reflect.ValueOf(source), "Tracker", sourceReferences)
	clonedReferences := make(map[uintptr]string)
	collectReferences(reflect.ValueOf(cloned), "Tracker", clonedReferences)

	var shared []string
	for address, path := range clonedReferences {
		if _, ok := sourceReferences[address]; ok {
			shared = append(shared, path)
		}
	}
	sort.Strings(shared)
	require.Empty(t, shared, "the copy shares these with the cached definition")
}

// fillDefinition sets every field under value to a non-zero value, each
// slice and map to one entry, so a reference the copy shares is visible.
// An interface holds a definition value nesting every shape a YAML
// document decodes to.
func fillDefinition(t *testing.T, value reflect.Value, path string) {
	t.Helper()
	switch value.Kind() {
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int64:
		value.SetInt(1)
	case reflect.Float64:
		value.SetFloat(1)
	case reflect.String:
		value.SetString("value")
	case reflect.Interface:
		value.Set(reflect.ValueOf(map[string]any{
			"list":    []any{"value", map[string]string{"key": "value"}},
			"strings": []string{"value"},
		}))
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		fillDefinition(t, value.Elem(), path)
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		fillDefinition(t, value.Index(0), path+"[0]")
	case reflect.Map:
		key := reflect.New(value.Type().Key()).Elem()
		fillDefinition(t, key, path)
		element := reflect.New(value.Type().Elem()).Elem()
		fillDefinition(t, element, path+"[key]")
		value.Set(reflect.MakeMap(value.Type()))
		value.SetMapIndex(key, element)
	case reflect.Struct:
		for i := range value.NumField() {
			field := value.Type().Field(i)
			require.True(t, field.IsExported(), "%s.%s cannot be filled by reflection", path, field.Name)
			fillDefinition(t, value.Field(i), path+"."+field.Name)
		}
	default:
		require.Failf(t, "unhandled kind", "%s is a %s", path, value.Kind())
	}
}

// collectReferences records the address of every slice, map and pointer
// under value by the path that reaches it. A pointer to a zero-size type
// is left out, since Go may give every such allocation the same address.
func collectReferences(value reflect.Value, path string, references map[uintptr]string) {
	switch value.Kind() {
	case reflect.Interface:
		if !value.IsNil() {
			collectReferences(value.Elem(), path, references)
		}
	case reflect.Pointer:
		if value.IsNil() || value.Type().Elem().Size() == 0 {
			return
		}
		references[value.Pointer()] = path
		collectReferences(value.Elem(), path, references)
	case reflect.Slice:
		if value.Len() == 0 {
			return
		}
		references[value.Pointer()] = path
		for i := range value.Len() {
			collectReferences(value.Index(i), path+"["+strconv.Itoa(i)+"]", references)
		}
	case reflect.Map:
		if value.IsNil() {
			return
		}
		references[value.Pointer()] = path
		iterator := value.MapRange()
		for iterator.Next() {
			collectReferences(iterator.Value(), path+"["+iterator.Key().String()+"]", references)
		}
	case reflect.Struct:
		for i := range value.NumField() {
			collectReferences(value.Field(i), path+"."+value.Type().Field(i).Name, references)
		}
	default:
	}
}
