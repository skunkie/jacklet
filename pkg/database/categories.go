// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
)

// categoryList is a row's categories as the "categories" column holds
// them: the ids joined by commas, in the order the scrape gave them.
type categoryList []int

// Value implements driver.Valuer.
func (c *categoryList) Value() (driver.Value, error) {
	ids := make([]string, len(*c))
	for i, id := range *c {
		ids[i] = strconv.Itoa(id)
	}
	return strings.Join(ids, ","), nil
}

// Scan implements sql.Scanner.
func (c *categoryList) Scan(src any) error {
	var text string
	switch value := src.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	default:
		return fmt.Errorf("categories: unexpected %T", src)
	}
	var ids categoryList
	if text != "" {
		for field := range strings.SplitSeq(text, ",") {
			id, err := strconv.Atoi(field)
			if err != nil {
				return fmt.Errorf("categories: %w", err)
			}
			ids = append(ids, id)
		}
	}
	*c = ids
	return nil
}

// categoryMatch is the condition a row meets when one of its categories is
// the one bound to it, as categoryNeedle writes it: the column is wrapped
// in commas so that every id, the first and last included, is matched
// whole, and 2040 never matches 20400.
const categoryMatch = "instr(',' || categories || ',', ?) > 0"

// categoryNeedle is the value categoryMatch is bound to for category.
func categoryNeedle(category string) string {
	return "," + category + ","
}
