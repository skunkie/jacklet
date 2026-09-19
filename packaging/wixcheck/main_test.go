// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

// checkSource runs the checks over one document.
func checkSource(t *testing.T, source string) []string {
	t.Helper()
	var all authoring
	if err := collect([]byte(source), &all); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return check(&all)
}

// requireProblem fails unless exactly one problem was reported and it
// mentions want.
func requireProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	if len(problems) != 1 {
		t.Fatalf("want one problem mentioning %q, got %v", want, problems)
	}
	if !strings.Contains(problems[0], want) {
		t.Errorf("want a problem mentioning %q, got %q", want, problems[0])
	}
}

// requireClean fails if anything was reported.
func requireClean(t *testing.T, problems []string) {
	t.Helper()
	if len(problems) != 0 {
		t.Fatalf("want no problems, got %v", problems)
	}
}

func TestCheck_AcceptsSoundAuthoring(t *testing.T) {
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
}

// A component whose guid is generated from its keypath cannot hold
// several unversioned files.
func TestCheck_RejectsSeveralFilesWithoutAGuid(t *testing.T) {
	problems := checkSource(t, `
<Wix><Component Id="Readme"><File Id="a" KeyPath="yes" /><File Id="b" /></Component></Wix>`)
	requireProblem(t, problems, "component Readme has 2 files and no explicit Guid")
}

func TestCheck_RejectsAComponentWithNoKeyPath(t *testing.T) {
	problems := checkSource(t, `<Wix><Component Id="Empty" Guid="G" /></Wix>`)
	requireProblem(t, problems, "component Empty has no KeyPath")
}

// The defect that made an operator's port be overwritten on every upgrade
// and every repair: the guard reads as a guard and can never hold.
func TestCheck_RejectsAnEmptinessGuardOnAPropertyWithADefault(t *testing.T) {
	problems := checkSource(t, `
<Wix><Fragment>
  <Property Id="JACKLETPORT" Value="9117" />
  <SetProperty Id="JACKLETPORT" Action="RecoverPort" Value="[EXISTINGPORT]"
               Condition='JACKLETPORT="" AND EXISTINGPORT&lt;&gt;""' />
</Fragment></Wix>`)
	requireProblem(t, problems, "assignment RecoverPort is guarded on JACKLETPORT being empty")
}

func TestCheck_AllowsAGuardOnAPropertyThatCanBeEmpty(t *testing.T) {
	requireClean(t, checkSource(t, `
<Wix><Fragment>
  <Property Id="JACKLETPORT" />
  <SetProperty Id="JACKLETPORT" Action="RecoverPort" Value="[EXISTINGPORT]"
               Condition='JACKLETPORT="" AND EXISTINGPORT&lt;&gt;""' />
</Fragment></Wix>`))
}

// A check box is ticked whenever its property has any value, so a default
// offers the option as already chosen.
func TestCheck_RejectsACheckBoxBoundToAPropertyWithADefault(t *testing.T) {
	problems := checkSource(t, `
<Wix><Fragment>
  <Property Id="ADDFIREWALLRULE" Value="0" />
  <UI Id="TheUI"><Control Id="FirewallCheck" Type="CheckBox" Property="ADDFIREWALLRULE" /></UI>
  <UIRef Id="TheUI" />
</Fragment></Wix>`)
	requireProblem(t, problems, "check box FirewallCheck is bound to ADDFIREWALLRULE")
}

// The defect that ran the stock wizard in place of the settings page for
// three releases without a word.
func TestCheck_RejectsAUIThatNothingReferences(t *testing.T) {
	problems := checkSource(t, `<Wix><Fragment><UI Id="JackletConfigUI" /></Fragment></Wix>`)
	requireProblem(t, problems, "UI JackletConfigUI is never referenced")
}

func TestCheck_RejectsAnUnhiddenCredential(t *testing.T) {
	problems := checkSource(t, `<Wix><Property Id="JACKLETADMINPASSWORD" /></Wix>`)
	requireProblem(t, problems, "property JACKLETADMINPASSWORD looks like a credential")
}

// A hash is stored precisely so the credential need not be, and the
// property that recovers one holds whatever is already installed.
func TestCheck_AllowsAHashAndARecoveredValue(t *testing.T) {
	requireClean(t, checkSource(t, `
<Wix><Fragment>
  <Property Id="JACKLETADMINPASSWORDHASH" />
  <Property Id="EXISTINGAPIKEY" />
</Fragment></Wix>`))
}

// The real authoring is the case that matters most, so it is checked as
// part of the suite rather than only by the command.
func TestCheck_TheInstallerAuthoringIsSound(t *testing.T) {
	var all authoring
	for _, path := range []string{"../windows/jacklet.wxs", "../windows/jacklet.ui.wxs"} {
		if err := read(path, &all); err != nil {
			t.Fatalf("%v", err)
		}
	}
	requireClean(t, check(&all))
}

// checkMoves runs the reachability rule over hand-made rows, which is the
// half of the wizard this project does not write: the toolset's dialog
// library supplies the rest, and the two only meet once a package is
// linked.
func checkMoves(t *testing.T, events []controlEvent) []string {
	t.Helper()
	return reachability(events)
}

// The defect that made the settings page unreachable in every build: a
// move published below the library's own, which names no condition.
func TestReachability_RejectsAMoveUnderAnUnconditionalOne(t *testing.T) {
	problems := checkMoves(t, []controlEvent{
		{Dialog: "InstallDirDlg", Control: "Next", Event: "NewDialog",
			Argument: "JackletConfigDlg", Condition: `JACKLETAPIKEY=""`, Ordering: 2, Source: "ours.wxs:1"},
		{Dialog: "InstallDirDlg", Control: "Next", Event: "NewDialog",
			Argument: "VerifyReadyDlg", Ordering: 4, Source: "library.wxs:1"},
	})
	requireProblem(t, problems, "only that one is ever published")
}

func TestReachability_AcceptsAMoveOverAnUnconditionalOne(t *testing.T) {
	requireClean(t, checkMoves(t, []controlEvent{
		{Dialog: "InstallDirDlg", Control: "Next", Event: "NewDialog",
			Argument: "VerifyReadyDlg", Ordering: 4, Source: "library.wxs:1"},
		{Dialog: "InstallDirDlg", Control: "Next", Event: "NewDialog",
			Argument: "JackletConfigDlg", Condition: `JACKLETAPIKEY=""`, Ordering: 5, Source: "ours.wxs:1"},
	}))
}

// Conditional moves do not shadow one another: any of them may be the one
// whose condition holds.
func TestReachability_AcceptsConditionalMovesInAnyOrder(t *testing.T) {
	requireClean(t, checkMoves(t, []controlEvent{
		{Dialog: "D", Control: "Back", Event: "NewDialog", Argument: "A", Condition: "NOT Installed", Ordering: 1},
		{Dialog: "D", Control: "Back", Event: "NewDialog", Argument: "B", Condition: "Installed", Ordering: 2},
	}))
}

// Events that are not dialog moves are published whatever else is, so an
// ordering below a move says nothing about them.
func TestReachability_IgnoresEventsThatAreNotMoves(t *testing.T) {
	requireClean(t, checkMoves(t, []controlEvent{
		{Dialog: "D", Control: "Next", Event: "SetTargetPath", Argument: "[DIR]", Ordering: 1},
		{Dialog: "D", Control: "Next", Event: "NewDialog", Argument: "Next", Ordering: 4},
	}))
}

// What an operator saw on the settings page: three labels reading
// "&API key", "&Port" and "pass&word", because the attribute that spells
// the accelerator and the attribute that disables it were both set.
func TestCheck_RejectsAnAmpersandUnderNoPrefix(t *testing.T) {
	problems := checkSource(t, `
<Wix><Control Id="PortLabel" Type="Text" NoPrefix="yes" Text="&amp;Port" /></Wix>`)
	requireProblem(t, problems, `control PortLabel sets NoPrefix and its text holds an ampersand`)
}

// Prose keeps NoPrefix, which is what it is for: without it an ampersand
// in a sentence would vanish and underline the letter after it.
func TestCheck_AllowsNoPrefixOnTextWithNoAmpersand(t *testing.T) {
	requireClean(t, checkSource(t, `
<Wix><Control Id="Note" Type="Text" NoPrefix="yes" Text="Nothing to escape here." /></Wix>`))
}

// A key the installer creates outlives the product unless the authoring
// gives it up, and what it leaves behind holds the product's settings.
func TestCheck_RejectsAKeyThatSurvivesTheUninstall(t *testing.T) {
	requireProblem(t, checkSource(t, `
<Wix><Fragment>
  <Component Id="Settings">
    <RegistryKey Root="HKLM" Key="SOFTWARE\Jacklet\Settings" ForceCreateOnInstall="yes">
      <RegistryValue Name="ApiKey" Type="string" Value="[JACKLETAPIKEY]" KeyPath="yes" />
    </RegistryKey>
  </Component>
</Fragment></Wix>`), `SOFTWARE\Jacklet\Settings`)
}

// A RegistryValue naming a key it does not declare is not authoring that
// key, so it is not the authoring that has to give it up either.
func TestCheck_AcceptsALooseRegistryValue(t *testing.T) {
	requireClean(t, checkSource(t, `
<Wix><Fragment>
  <Component Id="Hash">
    <RegistryValue Root="HKLM" Key="SOFTWARE\Jacklet\Settings" Name="AdminPasswordHash"
                   Type="string" Value="[HASH]" KeyPath="yes" />
  </Component>
</Fragment></Wix>`))
}
