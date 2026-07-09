// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && (unix || wasip1)

package os

import "strings"

func dstRootProcAbsLocked(r *dstRoot, name string) string {
	parts, suffix, err := splitPathInRoot(name, nil, nil)
	if err != nil {
		return ""
	}
	nodeStack := []*dstFSNode{r.node}
	stack := make([]string, 0, len(parts))
	invalidProc := false
	for _, part := range parts {
		if invalidProc {
			stack = append(stack, part)
			continue
		}
		switch part {
		case ".":
			if dstProcStackIsLeaf(stack) {
				invalidProc = true
				stack = append(stack, part)
			}
			continue
		case "..":
			if len(nodeStack) == 1 {
				return ""
			}
			nodeStack = nodeStack[:len(nodeStack)-1]
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			if dstProcStackIsLeaf(stack) {
				invalidProc = true
				stack = append(stack, part)
				continue
			}
			atDiskRoot := len(nodeStack) == 1 && nodeStack[0] == r.disk.root && len(stack) == 0
			if atDiskRoot && part == "proc" {
				stack = append(stack, part)
				continue
			}
			if len(stack) == 0 || stack[0] != "proc" {
				cur := nodeStack[len(nodeStack)-1]
				next := cur.entries[part]
				if next == nil || !next.isDir {
					return ""
				}
				nodeStack = append(nodeStack, next)
			}
			stack = append(stack, part)
		}
	}
	if len(stack) == 0 || stack[0] != "proc" {
		return ""
	}
	var b strings.Builder
	for _, part := range stack {
		b.WriteByte('/')
		b.WriteString(part)
	}
	if suffix != "" {
		b.WriteString(suffix)
	}
	return b.String()
}

func dstRootMkdirAllProcReservedLocked(r *dstRoot, name string) bool {
	if r.node != r.disk.root {
		return false
	}
	parts, _, err := splitPathInRoot(name, nil, nil)
	if err != nil {
		return false
	}
	stack := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case ".":
			continue
		case "..":
			if len(stack) == 0 {
				return false
			}
			stack = stack[:len(stack)-1]
		default:
			if len(stack) == 0 && part == "proc" {
				return true
			}
			stack = append(stack, part)
		}
	}
	return len(stack) > 0 && stack[0] == "proc"
}
