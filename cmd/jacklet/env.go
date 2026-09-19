// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"strings"
)

// envPrefix namespaces Jacklet's environment variables. A shared
// environment — several services in one compose file, or one systemd
// EnvironmentFile — is a shared namespace, where an unprefixed name like
// API_KEY or DB_PATH is liable to be another program's.
const envPrefix = "JACKLET_"

// environment reads settings from the process environment.
type environment struct{}

// get returns the value of envPrefix+name.
func (e *environment) get(name string) string {
	return os.Getenv(envPrefix + name)
}

// orDefault returns the value of name, or def when it is unset.
func (e *environment) orDefault(name, def string) string {
	if value := e.get(name); value != "" {
		return value
	}
	return def
}

// secret returns a credential, read from the file named by "<name>_FILE"
// when that is set and from "<name>" otherwise. Supplying both is an
// error.
//
// The file form suits a Docker or Kubernetes secret, which arrives as a
// mounted file, and keeps the value out of the environment, which every
// child process inherits and `docker inspect` prints.
func (e *environment) secret(name string) (string, error) {
	path := e.get(name + "_FILE")
	direct := e.get(name)

	switch {
	case path != "" && direct != "":
		return "", fmt.Errorf("%[1]s and %[1]s_FILE are both set; supply only one", name)
	case path == "":
		return direct, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s_FILE: %w", name, err)
	}
	// A file written by `echo` or an editor ends in a newline that is not
	// part of the credential. Other whitespace is left alone, since it may
	// genuinely be part of one.
	return strings.TrimRight(string(data), "\r\n"), nil
}
