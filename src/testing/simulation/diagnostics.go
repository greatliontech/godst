// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"time"
)

// Causal event kind codes — must match runtime's dstCausal* constants.
const (
	causalHostBoot             uint8 = 1
	causalHostClockRedecl      uint8 = 2
	causalProcStart            uint8 = 3
	causalProcExit             uint8 = 4
	causalCrashHost            uint8 = 5
	causalCrashProc            uint8 = 6
	causalPartition            uint8 = 10
	causalPartitionOneWay      uint8 = 11
	causalPartitionRefuse      uint8 = 12
	causalHealLink             uint8 = 13
	causalIsolateHost          uint8 = 14
	causalUnisolateHost        uint8 = 15
	causalConfigureLink        uint8 = 16
	causalConfigureLinkOneWay  uint8 = 17
	causalUnconfigureLink      uint8 = 18
	causalResetLinkConns       uint8 = 19
	causalResetProcessConns    uint8 = 20
	causalFailDisk             uint8 = 21
	causalHealDisk             uint8 = 22
	causalFailWriteback        uint8 = 23
	causalHealWriteback        uint8 = 24
	causalFailFile             uint8 = 25
	causalHealFile             uint8 = 26
	causalLimitDisk            uint8 = 27
	causalUnlimitDisk          uint8 = 28
	causalSlowDisk             uint8 = 29
	causalUnslowDisk           uint8 = 30
	causalCorruptFile          uint8 = 31
	causalStepClock            uint8 = 32
	causalDriftClock           uint8 = 33
	causalNetListen            uint8 = 40
	causalNetListenerClose     uint8 = 41
	causalNetDialEstablished   uint8 = 42
	causalNetDialRefused       uint8 = 43
	causalNetDialTimeout       uint8 = 44
	causalNetRetransmitTimeout uint8 = 45
	causalNetKeepaliveTimeout  uint8 = 46
	causalNetRSTDelivered      uint8 = 47
	causalFSEIO                uint8 = 50
	causalFSFsyncgateDrop      uint8 = 51
	causalFSENOSPC             uint8 = 52
	causalUDPBind              uint8 = 60
	causalUDPClose             uint8 = 61
	causalUDPRefused           uint8 = 62
	causalUDPDrop              uint8 = 63
)

// formatRunDiagnostics snapshots and renders the current run's failure-only
// diagnostics (causal event timeline, activated Sometimes fault points, live
// goroutines killed by normal Process exit, and ungranted bubble writes to
// standard output/error), prefixed with the seed header. Returns "" when the
// run recorded none.
func formatRunDiagnostics(seed uint64) string {
	body := formatRunDiagnosticsBody()
	if body == "" {
		return ""
	}
	return fmt.Sprintf("--- simulation diagnostics (seed %d) ---\n%s", seed, body)
}

// formatRunDiagnosticsBody renders the current run's causal-timeline,
// activated-Sometimes, killed-goroutine, and captured bubble stdio sections
// without an outer seed header so ExploreTest can attach per-schedule
// diagnostics under each Failure block.
func formatRunDiagnosticsBody() string {
	killMeta, killSeqs, killGoPCs, killStartPCs, killStates, droppedKills, stdioMeta, stdioData, stdioTruncated, sometimesNames, sometimesMeta, sometimesOverflow, causalMeta, causalStrs, causalOmitted := dstSnapshotRunDiagnostics()
	if len(causalMeta) == 0 && causalOmitted == 0 && len(killMeta) == 0 && droppedKills == 0 && len(stdioMeta) == 0 && stdioTruncated == 0 && len(sometimesNames) == 0 && sometimesOverflow == 0 {
		return ""
	}

	var b strings.Builder
	if len(causalMeta) > 0 || causalOmitted > 0 {
		b.WriteString("causal event timeline:\n")
		if causalOmitted > 0 {
			fmt.Fprintf(&b, "  ... (%d earlier causal event(s) omitted; showing last %d)\n", causalOmitted, len(causalMeta))
		}
		for i, meta := range causalMeta {
			seq := meta[0]
			at := time.Duration(int64(meta[1]))
			kind := uint8(meta[2])
			flags := uint8(meta[2] >> 8)
			hostA := uint32(meta[3])
			hostB := uint32(meta[3] >> 32)
			proc := uint32(meta[4])
			pid := int32(uint32(meta[4] >> 32))
			arg0 := int64(meta[5])
			arg1 := int64(meta[6])
			arg2 := int64(meta[7])
			origLen := int(meta[8])
			var str string
			if i < len(causalStrs) {
				str = causalStrs[i]
			}
			fmt.Fprintf(&b, "  #%d +%s: %s\n", seq, at, formatCausalEvent(kind, flags, hostA, hostB, proc, pid, arg0, arg1, arg2, str, origLen))
		}
	}
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

func formatCausalHost(host uint32) string {
	hostName, _ := nodeNames(host, 0)
	if hostName != "" {
		return fmt.Sprintf("%q", hostName)
	}
	if host == 0 {
		return "root"
	}
	return fmt.Sprintf("host#%d", host)
}

func formatCausalStr(prefix string, origLen int) string {
	if origLen > len(prefix) {
		return fmt.Sprintf("%q (truncated from %d bytes)", prefix, origLen)
	}
	return fmt.Sprintf("%q", prefix)
}

func formatFSOpName(op uint8) string {
	switch op {
	case 1:
		return "read"
	case 2:
		return "pread"
	case 3:
		return "write"
	case 4:
		return "pwrite"
	case 5:
		return "fsync"
	case 6:
		return "fdatasync"
	case 7:
		return "fallocate"
	case 8:
		return "mmap"
	case 9:
		return "create"
	case 10:
		return "mkdir"
	default:
		return fmt.Sprintf("op#%d", op)
	}
}

func formatCausalEvent(kind, flags uint8, hostA, hostB, proc uint32, pid int32, arg0, arg1, arg2 int64, str string, origLen int) string {
	switch kind {
	case causalHostBoot:
		hostName, _ := nodeNames(hostA, 0)
		var extra strings.Builder
		if origLen > 0 && (str != hostName || origLen > len(str)) {
			fmt.Fprintf(&extra, " hostname=%s", formatCausalStr(str, origLen))
		}
		if arg0 != 0 {
			fmt.Fprintf(&extra, " clockOffset=%s", time.Duration(arg0))
		}
		if arg1 != 0 {
			fmt.Fprintf(&extra, " driftPPB=%d", arg1)
		}
		return fmt.Sprintf("Host boot %s (boot=%d%s)", formatCausalHost(hostA), arg2, extra.String())
	case causalHostClockRedecl:
		hostName, _ := nodeNames(hostA, 0)
		var extra strings.Builder
		if origLen > 0 && (str != hostName || origLen > len(str)) {
			fmt.Fprintf(&extra, " hostname=%s", formatCausalStr(str, origLen))
		}
		return fmt.Sprintf("Host clock re-declared %s (clockOffset=%s driftPPB=%d%s)", formatCausalHost(hostA), time.Duration(arg0), arg1, extra.String())
	case causalProcStart:
		ns := ""
		if origLen > 0 {
			ns = " pidns=" + formatCausalStr(str, origLen)
		}
		return fmt.Sprintf("Process start [%s%s]", formatDiagNode(hostA, proc, pid), ns)
	case causalProcExit:
		reason := "normal"
		switch flags {
		case 1:
			reason = "panic"
		case 2:
			reason = "mapping-fault"
		}
		return fmt.Sprintf("Process exit [%s] (reason=%s)", formatDiagNode(hostA, proc, pid), reason)
	case causalCrashHost:
		tear := "false"
		if flags&1 != 0 {
			tear = "true"
		}
		return fmt.Sprintf("CrashHost %s (procs=%d crashTear=%s)", formatCausalHost(hostA), arg0, tear)
	case causalCrashProc:
		if arg0 == 1 {
			return fmt.Sprintf("CrashProcess [%s]", formatDiagNode(hostA, proc, pid))
		}
		return fmt.Sprintf("CrashProcess [%s] (instances=%d)", formatDiagNode(hostA, proc, 0), arg0)
	case causalPartition:
		return fmt.Sprintf("Partition %s <-> %s", formatCausalHost(hostA), formatCausalHost(hostB))
	case causalPartitionOneWay:
		return fmt.Sprintf("PartitionOneWay %s -> %s", formatCausalHost(hostA), formatCausalHost(hostB))
	case causalPartitionRefuse:
		return fmt.Sprintf("PartitionRefuse %s <-> %s", formatCausalHost(hostA), formatCausalHost(hostB))
	case causalHealLink:
		return fmt.Sprintf("HealLink %s <-> %s", formatCausalHost(hostA), formatCausalHost(hostB))
	case causalIsolateHost:
		return fmt.Sprintf("IsolateHost %s", formatCausalHost(hostA))
	case causalUnisolateHost:
		return fmt.Sprintf("UnisolateHost %s", formatCausalHost(hostA))
	case causalConfigureLink:
		return fmt.Sprintf("ConfigureLink %s <-> %s (%s)", formatCausalHost(hostA), formatCausalHost(hostB), formatLinkConfigDiag(flags, arg0, arg1, arg2, str))
	case causalConfigureLinkOneWay:
		return fmt.Sprintf("ConfigureLinkOneWay %s -> %s (%s)", formatCausalHost(hostA), formatCausalHost(hostB), formatLinkConfigDiag(flags, arg0, arg1, arg2, str))
	case causalUnconfigureLink:
		return fmt.Sprintf("UnconfigureLink %s <-> %s", formatCausalHost(hostA), formatCausalHost(hostB))
	case causalResetLinkConns:
		return fmt.Sprintf("ResetLinkConns %s <-> %s", formatCausalHost(hostA), formatCausalHost(hostB))
	case causalResetProcessConns:
		return fmt.Sprintf("ResetProcessConns [%s] (instances=%d)", formatDiagNode(hostA, proc, 0), arg0)
	case causalFailDisk:
		return fmt.Sprintf("FailDisk %s", formatCausalHost(hostA))
	case causalHealDisk:
		return fmt.Sprintf("HealDisk %s", formatCausalHost(hostA))
	case causalFailWriteback:
		return fmt.Sprintf("FailWriteback %s", formatCausalHost(hostA))
	case causalHealWriteback:
		return fmt.Sprintf("HealWriteback %s", formatCausalHost(hostA))
	case causalFailFile:
		return fmt.Sprintf("FailFile %s path=%s", formatCausalHost(hostA), formatCausalStr(str, origLen))
	case causalHealFile:
		return fmt.Sprintf("HealFile %s path=%s", formatCausalHost(hostA), formatCausalStr(str, origLen))
	case causalLimitDisk:
		return fmt.Sprintf("LimitDisk %s (bytes=%d)", formatCausalHost(hostA), arg0)
	case causalUnlimitDisk:
		return fmt.Sprintf("UnlimitDisk %s", formatCausalHost(hostA))
	case causalSlowDisk:
		return fmt.Sprintf("SlowDisk %s (perOp=%s)", formatCausalHost(hostA), time.Duration(arg0))
	case causalUnslowDisk:
		return fmt.Sprintf("UnslowDisk %s", formatCausalHost(hostA))
	case causalCorruptFile:
		target := "durable"
		if flags&1 != 0 {
			target = "offline-platter"
		}
		return fmt.Sprintf("CorruptFile %s path=%s (offset=%d bit=%d image=%s)", formatCausalHost(hostA), formatCausalStr(str, origLen), arg0, arg1, target)
	case causalStepClock:
		return fmt.Sprintf("StepClock %s (delta=%s)", formatCausalHost(hostA), time.Duration(arg0))
	case causalDriftClock:
		return fmt.Sprintf("DriftClock %s (ppb=%d)", formatCausalHost(hostA), arg0)
	case causalNetListen:
		return fmt.Sprintf("TCP Listen [%s] addr=%s (port=%d)", formatDiagNode(hostA, proc, pid), formatCausalStr(str, origLen), arg0)
	case causalNetListenerClose:
		return fmt.Sprintf("TCP Listener.Close [%s] addr=%s (port=%d)", formatDiagNode(hostA, proc, pid), formatCausalStr(str, origLen), arg0)
	case causalNetDialEstablished:
		return fmt.Sprintf("TCP Dial established [%s] :%d -> %s:%d (addr=%s)", formatDiagNode(hostA, proc, pid), arg0, formatCausalHost(hostB), arg1, formatCausalStr(str, origLen))
	case causalNetDialRefused:
		return fmt.Sprintf("TCP Dial refused [%s] -> %s (addr=%s)", formatDiagNode(hostA, proc, pid), formatCausalHost(hostB), formatCausalStr(str, origLen))
	case causalNetDialTimeout:
		reason := "deadline"
		if flags&1 != 0 {
			reason = "retransmit"
		}
		return fmt.Sprintf("TCP Dial timeout [%s] -> %s (addr=%s reason=%s)", formatDiagNode(hostA, proc, pid), formatCausalHost(hostB), formatCausalStr(str, origLen), reason)
	case causalNetRetransmitTimeout:
		return fmt.Sprintf("TCP retransmit ETIMEDOUT %s:%d -> %s:%d", formatCausalHost(hostA), arg0, formatCausalHost(hostB), arg1)
	case causalNetKeepaliveTimeout:
		return fmt.Sprintf("TCP keepalive ETIMEDOUT %s:%d -> %s:%d", formatCausalHost(hostA), arg0, formatCausalHost(hostB), arg1)
	case causalNetRSTDelivered:
		return fmt.Sprintf("TCP RST delivered %s:%d <- %s:%d", formatCausalHost(hostA), arg0, formatCausalHost(hostB), arg1)
	case causalFSEIO:
		return fmt.Sprintf("FS EIO [%s] op=%s path=%s", formatDiagNode(hostA, proc, pid), formatFSOpName(flags), formatCausalStr(str, origLen))
	case causalFSFsyncgateDrop:
		return fmt.Sprintf("FS fsyncgate writeback drop [%s] op=%s path=%s", formatDiagNode(hostA, proc, pid), formatFSOpName(flags), formatCausalStr(str, origLen))
	case causalFSENOSPC:
		if arg1 > 0 {
			return fmt.Sprintf("FS ENOSPC [%s] op=%s path=%s (written=%d requested=%d)", formatDiagNode(hostA, proc, pid), formatFSOpName(flags), formatCausalStr(str, origLen), arg0, arg1)
		}
		return fmt.Sprintf("FS ENOSPC [%s] op=%s path=%s", formatDiagNode(hostA, proc, pid), formatFSOpName(flags), formatCausalStr(str, origLen))
	case causalUDPBind:
		return fmt.Sprintf("UDP Bind [%s] addr=%s (port=%d)", formatDiagNode(hostA, proc, pid), formatCausalStr(str, origLen), arg0)
	case causalUDPClose:
		return fmt.Sprintf("UDP Close [%s] addr=%s (port=%d)", formatDiagNode(hostA, proc, pid), formatCausalStr(str, origLen), arg0)
	case causalUDPRefused:
		return fmt.Sprintf("UDP ECONNREFUSED %s:%d <- %s:%d", formatCausalHost(hostA), arg0, formatCausalHost(hostB), arg1)
	case causalUDPDrop:
		reason := "unknown"
		switch flags {
		case 1:
			reason = "partition"
		case 2:
			reason = "packet-loss"
		case 3:
			reason = "recv-buffer-full"
		}
		return fmt.Sprintf("UDP datagram dropped %s:%d -> %s:%d (reason=%s)", formatCausalHost(hostA), arg0, formatCausalHost(hostB), arg1, reason)
	default:
		return fmt.Sprintf("event#%d", kind)
	}
}

func diagUint64LE(s string, off int) uint64 {
	return uint64(s[off]) |
		uint64(s[off+1])<<8 |
		uint64(s[off+2])<<16 |
		uint64(s[off+3])<<24 |
		uint64(s[off+4])<<32 |
		uint64(s[off+5])<<40 |
		uint64(s[off+6])<<48 |
		uint64(s[off+7])<<56
}

func formatLinkConfigDiag(flags uint8, latencyNs, jitterNs, bandwidthBps int64, str string) string {
	base := fmt.Sprintf("latency=%s jitter=%s bandwidth=%dB/s", time.Duration(latencyNs), time.Duration(jitterNs), bandwidthBps)
	if flags == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	if flags&2 != 0 && len(str) >= 8 {
		fmt.Fprintf(&b, " loss=%g", math.Float64frombits(diagUint64LE(str, 0)))
	}
	if flags&4 != 0 && len(str) >= 16 {
		fmt.Fprintf(&b, " dup=%g", math.Float64frombits(diagUint64LE(str, 8)))
	}
	if flags&1 != 0 {
		b.WriteString(" reorder=true")
	}
	return b.String()
}
