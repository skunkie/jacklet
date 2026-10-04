// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Command mkman renders the manual pages the Debian and RPM packages
// install.
//
// jacklet(1) has a source of its own, because a manual page's shape --
// NAME, SYNOPSIS, OPTIONS -- is not the shape of anything in docs/. The
// section 7 pages are the docs/ files themselves, adapted here rather than
// in the files, so that what a reader sees on the project page stays what
// the authors wrote: the documentation index backlink and the relative
// links between pages are meaningless once installed, and a manual page
// needs a .TH line and a NAME section that markdown has nowhere to put.
//
// The adaptation assumes the shape those files have today, and says so
// when it stops holding: a link it cannot turn into a manual page
// reference, or a docs/ page it has never been told about, is an error
// rather than something rendered wrong and shipped.
//
// It lives in its own module so that go-md2man, and the markdown parser
// underneath it, stay out of Jacklet's own dependencies.
package main
