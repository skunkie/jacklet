// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"flag"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// manPage is the source of jacklet(1). It restates the flag set in prose,
// because a manual page's OPTIONS section is written for a reader rather
// than generated from the flags, which leaves it the one description of
// the settings that nothing else keeps in step: -help and the
// documentation both change with the code, while the installed manual page
// would quietly go on describing the flags of an older release.
const manPage = "../../packaging/jacklet.1.md"

// manPageFlag matches a flag as the page marks one up: **-port**.
var manPageFlag = regexp.MustCompile(`\*\*(-[a-z-]+)\*\*`)

// envName is the JACKLET_ variable a flag mirrors.
func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func TestManPageDescribesEverySetting(t *testing.T) {
	source, err := os.ReadFile(manPage)
	require.NoError(t, err)
	page := string(source)

	fs, _, _ := newSettingsFlags(io.Discard)

	fs.VisitAll(func(f *flag.Flag) {
		require.Contains(t, page, "**-"+f.Name+"**",
			"the manual page does not describe -%s under OPTIONS", f.Name)
		require.Contains(t, page, envName(f.Name),
			"the manual page does not name %s under ENVIRONMENT", envName(f.Name))
	})

	for _, secret := range secretEnvOnly {
		require.Contains(t, page, envPrefix+secret.name,
			"the manual page does not name the %s credential", secret.name)
	}
}

func TestManPageDescribesNothingThatIsGone(t *testing.T) {
	source, err := os.ReadFile(manPage)
	require.NoError(t, err)

	fs, _, _ := newSettingsFlags(io.Discard)

	for _, match := range manPageFlag.FindAllStringSubmatch(string(source), -1) {
		documented := match[1]
		// -help and -version are spellings the flag set never sees; they
		// are dispatched before it parses.
		if isInfoFlag(documented) {
			continue
		}
		require.NotNil(t, fs.Lookup(strings.TrimPrefix(documented, "-")),
			"the manual page describes %s, which is no longer a flag", documented)
	}
}
