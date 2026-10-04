// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"maps"
	"slices"
)

func cloneTracker(source *Tracker) Tracker {
	cloned := *source
	cloned.Caps = cloneCaps(source.Caps)
	cloned.Certificates = slices.Clone(source.Certificates)
	cloned.Download = clonePointer(source.Download, cloneDownloadBlock)
	cloned.LegacyLinks = slices.Clone(source.LegacyLinks)
	cloned.Links = slices.Clone(source.Links)
	cloned.Login = cloneLogin(source.Login)
	cloned.Replaces = slices.Clone(source.Replaces)
	cloned.Search = cloneSearch(source.Search)
	cloned.Settings = cloneSettings(source.Settings)
	cloned.TestLinkTorrent = clonePointer(source.TestLinkTorrent, func(value bool) bool { return value })
	return cloned
}

// clonePointer returns a new pointer to a copy of what source points to,
// made by clone, or nil for nil.
func clonePointer[T any](source *T, clone func(T) T) *T {
	if source == nil {
		return nil
	}
	cloned := clone(*source)
	return &cloned
}

func cloneDownloadBlock(source DownloadBlock) DownloadBlock {
	cloned := source
	cloned.Before = clonePointer(source.Before, cloneDownloadBefore)
	cloned.InfoHash = clonePointer(source.InfoHash, cloneDownloadInfoHash)
	cloned.Selectors = slices.Clone(source.Selectors)
	for i := range cloned.Selectors {
		cloned.Selectors[i] = cloneDownloadSelector(source.Selectors[i])
	}
	return cloned
}

func cloneDownloadBefore(source DownloadBefore) DownloadBefore {
	cloned := source
	cloned.Inputs = maps.Clone(source.Inputs)
	cloned.PathSelector = clonePointer(source.PathSelector, cloneDownloadSelector)
	return cloned
}

func cloneDownloadInfoHash(source DownloadInfoHash) DownloadInfoHash {
	cloned := source
	cloned.Hash = cloneDownloadSelector(source.Hash)
	cloned.Title = cloneDownloadSelector(source.Title)
	return cloned
}

func cloneDownloadSelector(source DownloadSelector) DownloadSelector {
	cloned := source
	cloned.Filters = cloneFilters(source.Filters)
	return cloned
}

func cloneCaps(source Caps) Caps {
	cloned := source
	cloned.CategoryMappings = slices.Clone(source.CategoryMappings)
	cloned.Modes = cloneModes(source.Modes)
	return cloned
}

func cloneModes(source map[string][]string) map[string][]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string][]string, len(source))
	for name, params := range source {
		cloned[name] = slices.Clone(params)
	}
	return cloned
}

func cloneLogin(source *Login) *Login {
	if source == nil {
		return nil
	}
	cloned := *source
	if source.Captcha != nil {
		cloned.Captcha = &Captcha{}
	}
	cloned.Cookies = slices.Clone(source.Cookies)
	cloned.Error = cloneErrorBlocks(source.Error)
	cloned.Inputs = maps.Clone(source.Inputs)
	if source.SelectorInputs != nil {
		cloned.SelectorInputs = make(map[string]Field, len(source.SelectorInputs))
		for name := range source.SelectorInputs {
			cloned.SelectorInputs[name] = cloneField(source.SelectorInputs[name])
		}
	}
	if source.Test != nil {
		test := *source.Test
		cloned.Test = &test
	}
	return &cloned
}

func cloneSearch(source Search) Search {
	cloned := source
	cloned.Error = cloneErrorBlocks(source.Error)
	cloned.Fields = cloneFieldList(source.Fields)
	cloned.Headers = cloneAnyMap(source.Headers)
	cloned.Inputs = maps.Clone(source.Inputs)
	cloned.KeywordsFilters = cloneFilters(source.KeywordsFilters)
	cloned.Paths = cloneSearchPaths(source.Paths)
	cloned.PreprocessingFilters = cloneFilters(source.PreprocessingFilters)
	cloned.Rows = cloneRows(source.Rows)
	return cloned
}

func cloneErrorBlocks(source []ErrorBlock) []ErrorBlock {
	cloned := slices.Clone(source)
	for i := range cloned {
		cloned[i].Message = cloneField(source[i].Message)
	}
	return cloned
}

func cloneFieldList(source FieldList) FieldList {
	cloned := slices.Clone(source)
	for i := range cloned {
		cloned[i].Field = cloneField(source[i].Field)
		cloned[i].Modifiers = slices.Clone(source[i].Modifiers)
	}
	return cloned
}

func cloneField(source Field) Field {
	cloned := source
	cloned.Case = slices.Clone(source.Case)
	cloned.Filters = cloneFilters(source.Filters)
	return cloned
}

func cloneFilters(source []Filter) []Filter {
	cloned := slices.Clone(source)
	for i := range cloned {
		cloned[i].Args = cloneDefinitionValue(source[i].Args)
	}
	return cloned
}

func cloneSearchPaths(source []SearchPath) []SearchPath {
	cloned := slices.Clone(source)
	for i := range cloned {
		cloned[i].Categories = slices.Clone(source[i].Categories)
		if source[i].InheritInputs != nil {
			shouldInheritInputs := *source[i].InheritInputs
			cloned[i].InheritInputs = &shouldInheritInputs
		}
		cloned[i].Inputs = maps.Clone(source[i].Inputs)
		if source[i].Response != nil {
			response := *source[i].Response
			cloned[i].Response = &response
		}
	}
	return cloned
}

func cloneRows(source Rows) Rows {
	cloned := source
	cloned.Count = clonePointer(source.Count, cloneField)
	cloned.DateHeaders = clonePointer(source.DateHeaders, cloneField)
	cloned.Filters = cloneFilters(source.Filters)
	return cloned
}

func cloneSettings(source []Setting) []Setting {
	cloned := slices.Clone(source)
	for i := range cloned {
		cloned[i].Default = cloneDefinitionValue(source[i].Default)
		cloned[i].Options = maps.Clone(source[i].Options)
	}
	return cloned
}

func cloneAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneDefinitionValue(value)
	}
	return cloned
}

func cloneDefinitionValue(source any) any {
	switch value := source.(type) {
	case map[string]any:
		return cloneAnyMap(value)
	case map[string]string:
		return maps.Clone(value)
	case []any:
		cloned := make([]any, len(value))
		for i := range value {
			cloned[i] = cloneDefinitionValue(value[i])
		}
		return cloned
	case []string:
		return slices.Clone(value)
	default:
		return value
	}
}
