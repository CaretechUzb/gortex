#!/bin/sh
# Guard for the linux "not actually a static binary" regression.
#
# README promises a "single static binary ... no dependency chain", but the
# linux builds linked libstdc++/libgcc/libc dynamically. That stamped them with
# the goreleaser-cross image's symbol-version floor (Ubuntu 24.04, GCC 13.3 ->
# GLIBCXX_3.4.32 and GLIBC_2.34), so the binary refused to even start anywhere
# older:
#
#   gortex: /lib/x86_64-linux-gnu/libstdc++.so.6:
#           version `GLIBCXX_3.4.32' not found (required by gortex)
#
# Debian 12 caps at GLIBCXX_3.4.30 and Ubuntu 22.04 at 3.4.29, so in practice
# the release only ran on Ubuntu 24.04+/Debian 13+. libstdc++ is in the link at
# all because some tree-sitter grammars ship C++ external scanners.
#
# .goreleaser.yml now links -static (with the netgo/osusergo tags so nothing
# needs a runtime dlopen of libnss_*). This script is the gate that keeps it
# that way: it runs as a goreleaser build post-hook, before any artifact is
# archived or published, and in CI on every change.
#
# Usage: verify-static-elf.sh <elf-binary>
set -eu

bin="${1:?usage: verify-static-elf.sh <elf-binary>}"
[ -f "$bin" ] || { echo "FATAL: $bin does not exist" >&2; exit 1; }

# GNU readelf reads foreign-architecture ELF fine, so one host readelf covers
# both the amd64 and the cross-linked arm64 binary. Never skip the check when
# it is missing — silently skipping is exactly how the broken shape shipped.
readelf=""
for cand in readelf llvm-readelf eu-readelf; do
	if command -v "$cand" >/dev/null 2>&1; then readelf="$cand"; break; fi
done
[ -n "$readelf" ] || { echo "FATAL: no readelf available to verify $bin" >&2; exit 1; }

fail=0

# 1. A statically linked binary has no DT_NEEDED entries at all.
needed="$("$readelf" -d "$bin" 2>/dev/null | grep NEEDED || true)"
if [ -n "$needed" ]; then
	echo "FATAL: $bin links shared libraries — static link incomplete:" >&2
	echo "$needed" >&2
	fail=1
fi

# 2. ... and no PT_INTERP segment, i.e. no dynamic loader is invoked. A binary
#    can lose its NEEDED entries and still be dynamically loaded.
if "$readelf" -l "$bin" 2>/dev/null | grep -q INTERP; then
	echo "FATAL: $bin has a PT_INTERP segment — it is not statically linked" >&2
	fail=1
fi

# 3. ... and requires no versioned symbols. This is the check that maps
#    directly to the reported failure: a partially static link can drop the
#    NEEDED entry while still versioning symbols against the build host.
vers="$("$readelf" -V "$bin" 2>/dev/null | grep -oE 'GLIBCXX_[0-9.]+|GLIBC_[0-9.]+|CXXABI_[0-9.]+' | sort -u || true)"
if [ -n "$vers" ]; then
	echo "FATAL: $bin carries symbol-version requirements (a distro floor):" >&2
	echo "$vers" | sed 's/^/       /' >&2
	fail=1
fi

[ "$fail" -eq 0 ] || exit 1

echo "ok: $bin is statically linked (no NEEDED, no PT_INTERP, no glibc/libstdc++ floor)"
