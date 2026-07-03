// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && (unix || (js && wasm) || plan9 || wasip1)

package syscall

// Stock build: the per-process environment view folds away. dstEnvEnabled is a
// constant false, so every `if dstEnvEnabled && …` branch in env_unix.go is
// dead-code-eliminated; the dispatch stubs below exist only so those branches
// type-check.

const dstEnvEnabled = false

func dstGetenv(key string) (value string, found, handled bool)   { return "", false, false }
func dstSetenv(key, value string) (err error, handled bool)      { return nil, false }
func dstUnsetenv(key string) (err error, handled bool)           { return nil, false }
func dstClearenv() (handled bool)                                { return false }
func dstEnviron() (env []string, handled bool)                   { return nil, false }
