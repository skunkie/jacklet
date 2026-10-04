// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// checkSource runs the checks over one document.
func checkSource(t *testing.T, source string) []string {
	t.Helper()
	var all authoring
	require.NoError(t, collect([]byte(source), &all))
	return check(&all)
}

// requireProblem fails unless exactly one problem was reported and it
// mentions want.
func requireProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	require.Len(t, problems, 1, "want one problem mentioning %q", want)
	require.Contains(t, problems[0], want)
}

// requireClean fails if anything was reported.
func requireClean(t *testing.T, problems []string) {
	t.Helper()
	require.Empty(t, problems)
}

func TestCheck(t *testing.T) {
	t.Run("accepts sound authoring", func(t *testing.T) {
		requireClean(t, checkSource(t, `
	<Wix><Fragment>
	  <Property Id="JACKLETAPIKEY" Hidden="yes" />
	  <Property Id="JACKLETPORT" />
	  <SetProperty Id="JACKLETPORT" Action="DefaultPort" Value="9117" Condition='JACKLETPORT=""' />
	  <UI Id="TheUI">
	    <Control Id="Box" Type="CheckBox" Property="ADDFIREWALLRULE" />
	  </UI>
	  <UIRef Id="TheUI" />
	  <Component Id="One"><File Id="a" KeyPath="yes" /></Component>
	  <Component Id="Two" Guid="G"><File Id="b" KeyPath="yes" /><File Id="c" /></Component>
	  <Component Id="Three"><CreateFolder /></Component>
	</Fragment></Wix>`))
	})

	// A component whose guid is generated from its keypath cannot hold
	// several unversioned files.
	t.Run("rejects several files without a guid", func(t *testing.T) {
		problems := checkSource(t, `
	<Wix><Component Id="Readme"><File Id="a" KeyPath="yes" /><File Id="b" /></Component></Wix>`)
		requireProblem(t, problems, "component Readme has 2 files and no explicit Guid")
	})

	t.Run("rejects a component with no key path", func(t *testing.T) {
		problems := checkSource(t, `<Wix><Component Id="Empty" Guid="G" /></Wix>`)
		requireProblem(t, problems, "component Empty has no KeyPath")
	})

	// A property with a default is never empty, so a guard on its being empty
	// never holds and the assignment it guards never runs: a value it was
	// meant to recover on an upgrade or repair is replaced by the default
	// instead.
	t.Run("rejects an emptiness guard on a property with a default", func(t *testing.T) {
		problems := checkSource(t, `
	<Wix><Fragment>
	  <Property Id="JACKLETPORT" Value="9117" />
	  <SetProperty Id="JACKLETPORT" Action="RecoverPort" Value="[EXISTINGPORT]"
	               Condition='JACKLETPORT="" AND EXISTINGPORT&lt;&gt;""' />
	</Fragment></Wix>`)
		requireProblem(t, problems, "assignment RecoverPort is guarded on JACKLETPORT being empty")
	})

	t.Run("allows a guard on a property that can be empty", func(t *testing.T) {
		requireClean(t, checkSource(t, `
	<Wix><Fragment>
	  <Property Id="JACKLETPORT" />
	  <SetProperty Id="JACKLETPORT" Action="RecoverPort" Value="[EXISTINGPORT]"
	               Condition='JACKLETPORT="" AND EXISTINGPORT&lt;&gt;""' />
	</Fragment></Wix>`))
	})

	// A check box is ticked whenever its property has any value, so a default
	// offers the option as already chosen.
	t.Run("rejects a check box bound to a property with a default", func(t *testing.T) {
		problems := checkSource(t, `
	<Wix><Fragment>
	  <Property Id="ADDFIREWALLRULE" Value="0" />
	  <UI Id="TheUI"><Control Id="FirewallCheck" Type="CheckBox" Property="ADDFIREWALLRULE" /></UI>
	  <UIRef Id="TheUI" />
	</Fragment></Wix>`)
		requireProblem(t, problems, "check box FirewallCheck is bound to ADDFIREWALLRULE")
	})

	// A UI that nothing references is dropped from the package without a
	// word, and the toolset's stock wizard runs in its place.
	t.Run("rejects a UI that nothing references", func(t *testing.T) {
		problems := checkSource(t, `<Wix><Fragment><UI Id="JackletConfigUI" /></Fragment></Wix>`)
		requireProblem(t, problems, "UI JackletConfigUI is never referenced")
	})

	t.Run("rejects an unhidden credential", func(t *testing.T) {
		problems := checkSource(t, `<Wix><Property Id="JACKLETADMINPASSWORD" /></Wix>`)
		requireProblem(t, problems, "property JACKLETADMINPASSWORD looks like a credential")
	})

	// A hash is stored precisely so the credential need not be, and the
	// property that recovers one holds whatever is already installed.
	t.Run("allows a hash and a recovered value", func(t *testing.T) {
		requireClean(t, checkSource(t, `
	<Wix><Fragment>
	  <Property Id="JACKLETADMINPASSWORDHASH" />
	  <Property Id="EXISTINGAPIKEY" />
	</Fragment></Wix>`))
	})

	// The real authoring is the case that matters most, so it is checked as
	// part of the suite rather than only by the command.
	t.Run("the installer authoring is sound", func(t *testing.T) {
		var all authoring
		for _, path := range []string{"../windows/jacklet.wxs", "../windows/jacklet.ui.wxs"} {
			require.NoError(t, read(path, &all))
		}
		requireClean(t, check(&all))
	})

	// A label that spells an accelerator with an ampersand and also sets the
	// attribute that disables accelerators shows the ampersand literally, as
	// "&Port" rather than "Port" with its P underlined.
	t.Run("rejects an ampersand under no prefix", func(t *testing.T) {
		problems := checkSource(t, `
	<Wix><Control Id="PortLabel" Type="Text" NoPrefix="yes" Text="&amp;Port" /></Wix>`)
		requireProblem(t, problems, `control PortLabel sets NoPrefix and its text holds an ampersand`)
	})

	// Prose keeps NoPrefix, which is what it is for: without it an ampersand
	// in a sentence would vanish and underline the letter after it.
	t.Run("allows no prefix on text with no ampersand", func(t *testing.T) {
		requireClean(t, checkSource(t, `
	<Wix><Control Id="Note" Type="Text" NoPrefix="yes" Text="Nothing to escape here." /></Wix>`))
	})

	// A key the installer creates outlives the product unless the authoring
	// gives it up, and what it leaves behind holds the product's settings.
	t.Run("rejects a key that survives the uninstall", func(t *testing.T) {
		requireProblem(t, checkSource(t, `
	<Wix><Fragment>
	  <Component Id="Settings">
	    <RegistryKey Root="HKLM" Key="SOFTWARE\Jacklet\Settings" ForceCreateOnInstall="yes">
	      <RegistryValue Name="ApiKey" Type="string" Value="[JACKLETAPIKEY]" KeyPath="yes" />
	    </RegistryKey>
	  </Component>
	</Fragment></Wix>`), `SOFTWARE\Jacklet\Settings`)
	})

	// A RegistryValue naming a key it does not declare is not authoring that
	// key, so it is not the authoring that has to give it up either.
	t.Run("accepts a loose registry value", func(t *testing.T) {
		requireClean(t, checkSource(t, `
	<Wix><Fragment>
	  <Component Id="Hash">
	    <RegistryValue Root="HKLM" Key="SOFTWARE\Jacklet\Settings" Name="AdminPasswordHash"
	                   Type="string" Value="[HASH]" KeyPath="yes" />
	  </Component>
	</Fragment></Wix>`))
	})
}

// checkMoves runs the reachability rule over hand-made rows, which is the
// half of the wizard this project does not write: the toolset's dialog
// library supplies the rest, and the two only meet once a package is
// linked.
func checkMoves(t *testing.T, events []controlEvent) []string {
	t.Helper()
	return reachability(events)
}

func TestReachability(t *testing.T) {
	// A move with no condition always applies, so a move ordered below the
	// library's unconditional one is never the one published, and the dialog
	// it leads to cannot be reached.
	t.Run("rejects a move under an unconditional one", func(t *testing.T) {
		problems := checkMoves(t, []controlEvent{
			{Dialog: "InstallDirDlg", Control: "Next", Event: "NewDialog",
				Argument: "JackletConfigDlg", Condition: `JACKLETAPIKEY=""`, Ordering: 2, Source: "ours.wxs:1"},
			{Dialog: "InstallDirDlg", Control: "Next", Event: "NewDialog",
				Argument: "VerifyReadyDlg", Ordering: 4, Source: "library.wxs:1"},
		})
		requireProblem(t, problems, "only that one is ever published")
	})

	t.Run("accepts a move over an unconditional one", func(t *testing.T) {
		requireClean(t, checkMoves(t, []controlEvent{
			{Dialog: "InstallDirDlg", Control: "Next", Event: "NewDialog",
				Argument: "VerifyReadyDlg", Ordering: 4, Source: "library.wxs:1"},
			{Dialog: "InstallDirDlg", Control: "Next", Event: "NewDialog",
				Argument: "JackletConfigDlg", Condition: `JACKLETAPIKEY=""`, Ordering: 5, Source: "ours.wxs:1"},
		}))
	})

	// Conditional moves do not shadow one another: any of them may be the one
	// whose condition holds.
	t.Run("accepts conditional moves in any order", func(t *testing.T) {
		requireClean(t, checkMoves(t, []controlEvent{
			{Dialog: "D", Control: "Back", Event: "NewDialog", Argument: "A", Condition: "NOT Installed", Ordering: 1},
			{Dialog: "D", Control: "Back", Event: "NewDialog", Argument: "B", Condition: "Installed", Ordering: 2},
		}))
	})

	// Events that are not dialog moves are published whatever else is, so an
	// ordering below a move says nothing about them.
	t.Run("ignores events that are not moves", func(t *testing.T) {
		requireClean(t, checkMoves(t, []controlEvent{
			{Dialog: "D", Control: "Next", Event: "SetTargetPath", Argument: "[DIR]", Ordering: 1},
			{Dialog: "D", Control: "Next", Event: "NewDialog", Argument: "Next", Ordering: 4},
		}))
	})
}
