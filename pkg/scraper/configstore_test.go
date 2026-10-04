// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper_test

import (
	"testing"

	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/admin/configtest"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// ConfigStore is the settings store Jacklet ships, so it has to meet the
// same contract an embedding program's implementation is held to, including
// keeping a credential that reads like a number a string.
func TestConfigStoreMeetsTheConfigContract(t *testing.T) {
	configtest.Run(t,
		func(t *testing.T) admin.Config {
			t.Helper()
			return scraper.NewConfigStore(t.TempDir())
		},
		func(t *testing.T) admin.Config {
			t.Helper()
			return scraper.NewConfigStore("")
		})
}
