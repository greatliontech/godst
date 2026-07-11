# Socketcall fence wrappers can stale stack-backed uintptr arguments

Lands: when 386 and s390x socketcall fence entry points cannot grow or move the
caller stack before kernel dispatch

## Gap

Severity H. The new Go `socketcall` and `rawsocketcall` wrappers are
splittable and accept pointer-derived `uintptr` arguments. A stack growth in
the wrapper moves stack objects without adjusting those integer arguments, so
the assembly dispatch can pass stale addresses to the kernel. Tagged non-bubble
socket operations can return `EFAULT`, read stale data, or corrupt reused stack
memory.

## Required outcome

The fence preserves the upstream pointer-lifetime contract on linux/386 and
linux/s390x. Structural tests reject `morestack` in both entry points, and a
deep-stack socket result-buffer test exercises the fallthrough path.
