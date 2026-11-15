// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// formatRunDiagnostics snapshots and renders the current run's failure-only
// diagnostics (activated Sometimes fault points, live goroutines killed by
// normal Process exit, and ungranted bubble writes to standard output/error),
// prefixed with the seed header. Returns "" when the run recorded none.
func formatRunDiagnostics(seed uint64) string {
	body := formatRunDiagnosticsBody()
	if body == "" {
		return ""
	}
	return fmt.Sprintf("--- simulation diagnostics (seed %d) ---\n%s", seed, body)
}

// formatRunDiagnosticsBody renders the current run's activated-Sometimes,
// killed-goroutine, and captured bubble stdio sections without an outer seed
// header so ExploreTest can attach per-schedule diagnostics under each Failure
// block.
func formatRunDiagnosticsBody() string {
	killMeta, killSeqs, killGoPCs, killStartPCs, killStates, droppedKills, stdioMeta, stdioData, stdioTruncated, sometimesNames, sometimesMeta, sometimesOverflow := dstSnapshotRunDiagnostics()
	if len(killMeta) == 0 && droppedKills == 0 && len(stdioMeta) == 0 && stdioTruncated == 0 && len(sometimesNames) == 0 && sometimesOverflow == 0 {
		return ""
	}

	var b strings.Builder
	if len(sometimesNames) > 0 || sometimesOverflow > 0 {
		b.WriteString("activated fault points (Sometimes):\n")
		for i, prefix := range sometimesNames {
			hits := sometimesMeta[i][0]
			firstAt := time.Duration(int64(sometimesMeta[i][1]))
			origLen := int(sometimesMeta[i][2])
			trunc := ""
			if origLen > len(prefix) {
				trunc = fmt.Sprintf(" (truncated from %d bytes)", origLen)
			}
			fmt.Fprintf(&b, "  %q%s: %d hit(s) (first at +%s)\n", prefix, trunc, hits, firstAt)
		}
		if sometimesOverflow > 0 {
			fmt.Fprintf(&b, "  ... +%d activation(s) across additional fault point(s) omitted\n", sometimesOverflow)
		}
	}

	if len(killMeta) > 0 || droppedKills > 0 {
		b.WriteString("killed goroutines on process exit:\n")
		for _, meta := range killMeta {
			host := uint32(meta[0])
			proc := uint32(meta[1])
			pid := int32(uint32(meta[2]))
			total := uint32(meta[3])
			startIdx := int(uint32(meta[4]))
			count := int(uint32(meta[4] >> 32))
			fmt.Fprintf(&b, "  %s: %d live goroutine(s) killed on process exit\n", formatDiagNode(host, proc, pid), total)
			for j := 0; j < count && startIdx+j < len(killSeqs); j++ {
				idx := startIdx + j
				fmt.Fprintf(&b, "    g#%d [%s]: %s\n",
					killSeqs[idx],
					killStates[idx],
					symbolizeKilledG(killGoPCs[idx], killStartPCs[idx]))
			}
			if int(total) > count {
				fmt.Fprintf(&b, "    ... +%d additional killed goroutine(s) omitted\n", int(total)-count)
			}
		}
		if droppedKills > 0 {
			fmt.Fprintf(&b, "  ... +%d process-exit kill event(s) omitted\n", droppedKills)
		}
	}

	if len(stdioMeta) > 0 || stdioTruncated > 0 {
		b.WriteString("captured bubble stdio:\n")
		for _, meta := range stdioMeta {
			host := uint32(meta[0])
			proc := uint32(meta[1])
			pid := int32(uint32(meta[2]))
			fd := uint8(meta[3])
			off := int(meta[4])
			length := int(meta[5])
			if off < 0 || length <= 0 || off+length > len(stdioData) {
				continue
			}
			stream := "stdout"
			if fd == 2 {
				stream = "stderr"
			}
			fmt.Fprintf(&b, "  [%s %s]\n", formatDiagNode(host, proc, pid), stream)
			writeIndentedStdio(&b, string(stdioData[off:off+length]))
		}
		if stdioTruncated > 0 {
			fmt.Fprintf(&b, "  ... (stdio truncated: %d byte(s) omitted)\n", stdioTruncated)
		}
	}

	return strings.TrimSuffix(b.String(), "\n")
}

func formatDiagNode(host, proc uint32, pid int32) string {
	hostName, procName := nodeNames(host, proc)
	var parts []string
	if hostName != "" {
		parts = append(parts, fmt.Sprintf("host=%q", hostName))
	} else if host != 0 {
		parts = append(parts, fmt.Sprintf("host#%d", host))
	}
	if procName != "" {
		parts = append(parts, fmt.Sprintf("proc=%q", procName))
	} else if proc != 0 {
		parts = append(parts, fmt.Sprintf("proc#%d", proc))
	}
	if len(parts) == 0 {
		parts = append(parts, "root")
	}
	if pid > 0 {
		parts = append(parts, fmt.Sprintf("pid=%d", pid))
	}
	return strings.Join(parts, " ")
}

func symbolizeKilledG(gopc, startpc uintptr) string {
	entry := symbolizeEntryPC(startpc)
	creator := symbolizeCallerPC(gopc)
	switch {
	case entry != "" && creator != "":
		return entry + " (created by " + creator + ")"
	case entry != "":
		return entry
	case creator != "":
		return "created by " + creator
	default:
		return fmt.Sprintf("pc=0x%x", startpc)
	}
}

func symbolizeEntryPC(pc uintptr) string {
	if pc == 0 {
		return ""
	}
	if fn := runtime.FuncForPC(pc); fn != nil {
		return fn.Name()
	}
	return ""
}

func symbolizeCallerPC(pc uintptr) string {
	if pc == 0 {
		return ""
	}
	frames := runtime.CallersFrames([]uintptr{pc})
	frame, _ := frames.Next()
	if frame.Function == "" {
		return ""
	}
	if frame.File != "" && frame.Line > 0 {
		return fmt.Sprintf("%s at %s:%d", frame.Function, shortDiagFile(frame.File), frame.Line)
	}
	return frame.Function
}

func shortDiagFile(file string) string {
	if i := strings.LastIndexByte(file, '/'); i >= 0 {
		return file[i+1:]
	}
	return file
}

func writeIndentedStdio(b *strings.Builder, s string) {
	trimmed := strings.TrimSuffix(s, "\n")
	for _, line := range strings.Split(trimmed, "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
}
