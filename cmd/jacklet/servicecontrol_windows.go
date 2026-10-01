// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// settingsKeyPath holds the settings an installed service runs with, one
// PascalCase registry value each.
//
// It is under Jacklet's own key rather than the service's, so the service
// account never needs write access to the key that controls its executable.
// Settings kept in the service key would also be destroyed during an
// upgrade, before the replacing package could carry them forward.
const settingsKeyPath = `SOFTWARE\` + serviceName + `\Settings`

// installerKeyPath holds bookkeeping that belongs to the installer rather
// than to the service's runtime configuration.
const installerKeyPath = `SOFTWARE\` + serviceName + `\Installer`

// serviceAccount is the unprivileged built-in account the service runs as.
// Jacklet needs to make outbound HTTP requests and write its own state
// directory, and nothing else.
const serviceAccount = `NT AUTHORITY\LocalService`

// controlTimeout bounds how long a start or stop verb waits for the
// Service Control Manager to reach the requested state.
//
// A variable, with controlPollInterval, so a test can reach the far side
// of the wait without spending it.
var controlTimeout = 45 * time.Second

// serviceUsage describes the service subcommand's verbs.
const serviceUsage = `jacklet service - manage the Windows service

Usage:
  jacklet service install     Register the service and its event log source.
  jacklet service uninstall   Remove the service, event log source, and registry settings.
  jacklet service start       Start the service.
  jacklet service stop        Stop the service.
  jacklet service status      Print the service state.
  jacklet service config      Print the configured settings, credentials masked.
  jacklet service config set NAME=VALUE
                              Set one setting. Written without a value, the
                              value is read from standard input, so a
                              credential stays out of the process list and
                              the shell history.
  jacklet service config unset NAME
                              Remove one setting.

Settings take effect when the service next starts, which is when they are
read. Names use Windows-style PascalCase, such as ApiKey, Port, and LogFile.
`

// runService dispatches a service verb.
func runService(args []string) error {
	if len(args) == 0 {
		fmt.Print(serviceUsage)
		return nil
	}

	switch args[0] {
	case "install":
		return installService()
	case "uninstall":
		return uninstallService()
	case "start":
		return startService()
	case "stop":
		return stopService()
	case "status":
		return printServiceStatus(os.Stdout)
	case "config":
		return runServiceConfig(args[1:], os.Stdin, os.Stdout)
	case "help", "-h", "-help", "--help":
		fmt.Print(serviceUsage)
		return nil
	default:
		return fmt.Errorf("unknown service command %q\n\n%s", args[0], serviceUsage)
	}
}

// elevationHint rewraps a permission error with what to do about it,
// because "Access is denied." on its own does not say that the command
// needed an elevated prompt.
func elevationHint(action string, err error) error {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s: %w (run this from an elevated command prompt)", action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

// The access each verb asks for, rather than everything each of them
// could want.
//
// mgr.Connect and mgr.OpenService ask for full control, which needs an
// elevated prompt whatever the verb goes on to do, so a verb that only
// reports the state would refuse to run for the operator most likely to
// want it. These are the rights the verb actually exercises, so status
// works from an ordinary prompt and the rest still say what they need.
const (
	queryAccess     = windows.SERVICE_QUERY_CONFIG | windows.SERVICE_QUERY_STATUS
	startAccess     = windows.SERVICE_QUERY_STATUS | windows.SERVICE_START
	stopAccess      = windows.SERVICE_QUERY_STATUS | windows.SERVICE_STOP
	uninstallAccess = stopAccess | windows.DELETE
)

// openServiceManager connects to the Service Control Manager with the
// rights the caller needs.
func openServiceManager(access uint32) (*mgr.Mgr, error) {
	handle, err := windows.OpenSCManager(nil, nil, access)
	if err != nil {
		return nil, elevationHint("connecting to the service control manager", err)
	}
	return &mgr.Mgr{Handle: handle}, nil
}

// openService opens the installed service with the rights the caller needs.
func openService(manager *mgr.Mgr, access uint32) (*mgr.Service, error) {
	name, err := windows.UTF16PtrFromString(serviceName)
	if err != nil {
		return nil, fmt.Errorf("naming the %s service: %w", serviceName, err)
	}
	// Asking the manager for no more than the verb needs moves the access
	// check from opening the manager to opening the service, so this is
	// where an unelevated prompt now fails. Telling those two apart is
	// what keeps "install it first" off a service that is installed and
	// merely out of reach.
	handle, err := windows.OpenService(manager.Handle, name, access)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, fmt.Errorf("opening the %s service: %w (run \"jacklet service install\" first)", serviceName, err)
	}
	if err != nil {
		return nil, elevationHint(fmt.Sprintf("opening the %s service", serviceName), err)
	}
	return &mgr.Service{Name: serviceName, Handle: handle}, nil
}

func installService() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolving the executable path: %w", err)
	}

	manager, err := openServiceManager(windows.SC_MANAGER_CONNECT | windows.SC_MANAGER_CREATE_SERVICE)
	if err != nil {
		return err
	}
	defer func() { _ = manager.Disconnect() }()

	// Only "it is not there" means there is room to install one. Any other
	// refusal is reported rather than read as an absence, which would send
	// the install on to create a service that already exists.
	switch service, err := openService(manager, queryAccess); {
	case err == nil:
		service.Close()
		return fmt.Errorf("the %s service is already installed", serviceName)
	case !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		return err
	}

	// The state directory is created before the service, so the account
	// the service runs as can write its database and its log from the
	// first start.
	if err := prepareStateDir(); err != nil {
		return err
	}

	service, err := manager.CreateService(serviceName, executable, mgr.Config{
		Description: serviceDescription,
		DisplayName: serviceDisplayName,
		// Manual, and not started: an unconfigured Jacklet serves its
		// indexer endpoints to anyone who can reach the port, so starting
		// it is a decision the operator makes after setting an API key.
		ServiceStartName: serviceAccount,
		StartType:        mgr.StartManual,
	})
	if err != nil {
		return elevationHint("creating the service", err)
	}
	defer service.Close()

	if err := configureRecovery(service); err != nil {
		return err
	}

	// Registered with the generic message file, because Jacklet carries no
	// message resource of its own; the event text is then the message
	// Jacklet writes.
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Info|eventlog.Warning); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("registering the event log source: %w", err)
	}

	if err := seedServiceConfiguration(); err != nil {
		return err
	}

	fmt.Printf("Installed the %s service.\n\n", serviceName)
	fmt.Print("Set an API key, then start it:\n" +
		"  jacklet service config set ApiKey\n" +
		"  jacklet service start\n")
	return nil
}

// recoveryConfigurer is the part of a service manager connection the recovery
// setup needs, so tests can substitute a recorder for an installed service.
type recoveryConfigurer interface {
	SetRecoveryActions(actions []mgr.RecoveryAction, resetPeriod uint32) error
	SetRecoveryActionsOnNonCrashFailures(enabled bool) error
}

// configureRecovery restarts the service after a failure, matching the
// restart-on-failure behavior the systemd unit configures.
func configureRecovery(service recoveryConfigurer) error {
	actions := []mgr.RecoveryAction{
		{Delay: 5 * time.Second, Type: mgr.ServiceRestart},
		{Delay: 5 * time.Second, Type: mgr.ServiceRestart},
		{Delay: 0, Type: mgr.NoAction},
	}
	if err := service.SetRecoveryActions(actions, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("configuring the service recovery actions: %w", err)
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("enabling recovery after a service error: %w", err)
	}
	return nil
}

// seedServiceConfiguration writes the absolute paths the service reads its
// state from, unless they are already configured.
//
// A service's working directory is the system directory, so the built-in
// working-directory-relative defaults would resolve somewhere nobody
// intends; naming them explicitly is what makes the service's state land
// beside itself.
func seedServiceConfiguration() error {
	config, err := readServiceConfiguration()
	if err != nil {
		return err
	}

	state := stateDir()
	for name, value := range map[string]string{
		"ConfigDir":      filepath.Join(state, "config"),
		"DatabasePath":   filepath.Join(state, "jacklet.db"),
		"DefinitionsDir": filepath.Join(state, "definitions"),
		"LogFile":        filepath.Join(state, "logs", "jacklet.log"),
	} {
		if _, ok := config.lookup(name); !ok {
			config = config.set(name, value)
		}
	}
	return writeServiceConfiguration(config)
}

func uninstallService() error {
	manager, err := openServiceManager(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer func() { _ = manager.Disconnect() }()

	service, err := openService(manager, uninstallAccess)
	if err != nil {
		return err
	}
	defer service.Close()

	// Returned as it stands: ensureStopped already names what failed, and
	// it runs before anything is removed, so there is no partial state to
	// warn about.
	if err := ensureStopped(service); err != nil {
		return err
	}

	// Before the service, because removing the service is what makes this
	// command unrepeatable: a second run cannot open a service that is
	// already gone, so anything that can fail belongs ahead of it.
	if err := eventlog.Remove(serviceName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("removing the event log source: %w", err)
	}
	if err := removeProductRegistry(registry.LOCAL_MACHINE); err != nil {
		return elevationHint("removing the service configuration", err)
	}
	if err := service.Delete(); err != nil {
		return elevationHint("removing the service", err)
	}

	// The state directory is left alone: the database, the definitions and
	// the per-indexer credentials in it are the operator's, not the
	// installation's.
	fmt.Printf("Removed the %s service and its registry settings. Its state remains in %s.\n",
		serviceName, stateDir())
	return nil
}

// removeProductRegistry deletes the service configuration and installer
// bootstrap state while leaving the operator's files under ProgramData alone.
func removeProductRegistry(root registry.Key) error {
	for _, path := range []string{bootstrapKeyPath, settingsKeyPath, installerKeyPath, productKeyPath} {
		if err := registry.DeleteKey(root, path); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
	}
	return nil
}

func startService() error {
	manager, err := openServiceManager(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer func() { _ = manager.Disconnect() }()

	service, err := openService(manager, startAccess)
	if err != nil {
		return err
	}
	defer service.Close()

	if err := service.Start(); err != nil {
		return elevationHint("starting the service", err)
	}
	if err := awaitState(service, svc.Running); err != nil {
		return err
	}
	fmt.Printf("Started the %s service.\n", serviceName)
	return nil
}

func stopService() error {
	manager, err := openServiceManager(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer func() { _ = manager.Disconnect() }()

	service, err := openService(manager, stopAccess)
	if err != nil {
		return err
	}
	defer service.Close()

	if err := ensureStopped(service); err != nil {
		return err
	}
	fmt.Printf("Stopped the %s service.\n", serviceName)
	return nil
}

// serviceController is the part of an installed service the state
// handling drives, so a test can exercise it without one.
type serviceController interface {
	Control(c svc.Cmd) (svc.Status, error)
	Query() (svc.Status, error)
}

// controlPollInterval is how often the service is asked whether it has
// reached the state a verb is waiting for.
var controlPollInterval = 300 * time.Millisecond

// ensureStopped leaves the service stopped, whatever state it is in now.
//
// The stop is issued only from a state that accepts one. The Service
// Control Manager refuses the control on a service that is already
// stopped, and on one that is still starting or already stopping, so
// asking unconditionally would report "it is already where you want it"
// as a failure -- which for the uninstall means refusing to remove a
// service that was merely mid-shutdown.
func ensureStopped(service serviceController) error {
	deadline := time.Now().Add(controlTimeout)
	requested := false
	for {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("querying the service: %w", err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		if !requested && status.State != svc.StartPending && status.State != svc.StopPending {
			// The state can still move between the query and the control
			// -- another stop, or the service exiting on its own. A
			// refusal on those grounds says what the next query would
			// have said, so it is read that way rather than as a failure
			// to stop a service that is already stopping.
			switch _, err := service.Control(svc.Stop); {
			case err == nil:
				requested = true
			case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL),
				errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
			default:
				return elevationHint("stopping the service", err)
			}
		}
		if time.Now().After(deadline) {
			return timedOut(status.State)
		}
		time.Sleep(controlPollInterval)
	}
}

// awaitState waits for the service to reach want, so a verb reports the
// outcome rather than the request.
func awaitState(service serviceController, want svc.State) error {
	deadline := time.Now().Add(controlTimeout)
	for {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("querying the service: %w", err)
		}
		if status.State == want {
			return nil
		}
		// A service that started and stopped again has finished; waiting
		// out the timeout would report it as slow rather than as failed.
		if want == svc.Running && status.State == svc.Stopped {
			return fmt.Errorf("the service stopped immediately after starting; check the event log and %s",
				filepath.Join(stateDir(), "logs"))
		}
		if time.Now().After(deadline) {
			return timedOut(status.State)
		}
		time.Sleep(controlPollInterval)
	}
}

// timedOut reports a service that never left state, pointing at the two
// places that say why.
func timedOut(state svc.State) error {
	return fmt.Errorf("the service was still %s after %s; check the event log and %s",
		stateName(state), controlTimeout, filepath.Join(stateDir(), "logs"))
}

func printServiceStatus(out io.Writer) error {
	manager, err := openServiceManager(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer func() { _ = manager.Disconnect() }()

	service, err := openService(manager, queryAccess)
	if err != nil {
		return err
	}
	defer service.Close()

	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("querying the service: %w", err)
	}
	config, err := service.Config()
	if err != nil {
		return fmt.Errorf("reading the service configuration: %w", err)
	}

	fmt.Fprintf(out, "%s\t%s\n", serviceName, stateName(status.State))
	fmt.Fprintf(out, "account\t%s\n", config.ServiceStartName)
	fmt.Fprintf(out, "start\t%s\n", startTypeName(config.StartType))
	return nil
}

// stateName renders a service state as the word "sc query" would print.
func stateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continuing"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown (%d)", state)
	}
}

func startTypeName(startType uint32) string {
	switch startType {
	case mgr.StartAutomatic:
		return "automatic"
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	default:
		return fmt.Sprintf("unknown (%d)", startType)
	}
}

// runServiceConfig reads or edits the settings the service applies at
// startup.
func runServiceConfig(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		config, err := readServiceConfiguration()
		if err != nil {
			return err
		}
		if len(config) == 0 {
			fmt.Fprintf(out, "The %s service has no settings configured.\n", serviceName)
			return nil
		}
		fmt.Fprint(out, config.String())
		return nil
	}

	switch args[0] {
	case "set":
		return setServiceSetting(args[1:], in, out)
	case "unset":
		return unsetServiceSetting(args[1:], out)
	default:
		return fmt.Errorf("unknown config command %q\n\n%s", args[0], serviceUsage)
	}
}

func setServiceSetting(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: jacklet service config set NAME[=VALUE]")
	}

	name, value, hasValue := strings.Cut(args[0], "=")
	name, err := settingName(name)
	if err != nil {
		return err
	}
	if !hasValue {
		// Read from standard input rather than taken as an argument: an
		// argument is readable by every other process on the machine and
		// lands in the shell history.
		value, err = readSettingValue(in, name)
		if err != nil {
			return err
		}
	}

	config, err := readServiceConfiguration()
	if err != nil {
		return err
	}
	if err := writeServiceConfiguration(config.set(name, value)); err != nil {
		return err
	}

	fmt.Fprintf(out, "Set %s. It takes effect when the service next starts.\n", name)
	return nil
}

// readSettingValue reads one line as a setting's value, prompting when
// standard input is a terminal.
func readSettingValue(in io.Reader, name string) (string, error) {
	if file, ok := in.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintf(os.Stderr, "Enter the value for %s, then press Enter.\n", name)
			fmt.Fprintln(os.Stderr, "Note: it will be visible as you type.")
		}
	}

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("no value was provided on standard input for %s", name)
	}
	return strings.TrimRight(scanner.Text(), "\r\n"), nil
}

func unsetServiceSetting(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: jacklet service config unset NAME")
	}
	name, err := settingName(args[0])
	if err != nil {
		return err
	}

	config, err := readServiceConfiguration()
	if err != nil {
		return err
	}
	if _, ok := config.lookup(name); !ok {
		return fmt.Errorf("%s is not configured", name)
	}
	if err := writeServiceConfiguration(config.unset(name)); err != nil {
		return err
	}

	fmt.Fprintf(out, "Removed %s. It takes effect when the service next starts.\n", name)
	return nil
}

// readServiceConfiguration reads the service's configured settings.
func readServiceConfiguration() (serviceConfiguration, error) {
	return readServiceSettings(registry.LOCAL_MACHINE)
}

// readServiceSettings reads the settings stored under root, which the
// tests point somewhere writable.
func readServiceSettings(root registry.Key) (serviceConfiguration, error) {
	key, err := registry.OpenKey(root, settingsKeyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening the settings key: %w", err)
	}
	defer key.Close()

	names, err := key.ReadValueNames(0)
	if err != nil {
		return nil, fmt.Errorf("reading the settings: %w", err)
	}

	config := make(serviceConfiguration, 0, len(names))
	for _, name := range names {
		setting, ok := findServiceSetting(name)
		if !ok {
			continue
		}
		value, _, err := key.GetStringValue(name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		config = config.set(setting.Name, value)
	}
	return config, nil
}

// writeServiceConfiguration stores the service's settings, one registry
// value each, removing any that are no longer configured.
func writeServiceConfiguration(config serviceConfiguration) error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, settingsKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return elevationHint("opening the settings key for writing", err)
	}
	defer key.Close()

	existing, err := key.ReadValueNames(0)
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}

	for _, entry := range config {
		name, value, ok := splitSetting(entry)
		if !ok {
			continue
		}
		if err := key.SetStringValue(name, value); err != nil {
			return elevationHint("writing "+name, err)
		}
	}

	for _, name := range existing {
		setting, known := findServiceSetting(name)
		if !known {
			continue
		}
		if _, ok := config.lookup(setting.Name); ok {
			continue
		}
		if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", name, err)
		}
	}
	return nil
}
