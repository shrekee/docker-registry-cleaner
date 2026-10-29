#!/usr/bin/env python3
"""Build and check release artifacts from a separately checked-out tag.

The source checkout is never asked to supply release automation. Its commit must
match the full commit ID passed by the caller, and its working tree must be clean.
"""
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import zlib

TARGETS = (("linux", "amd64"), ("linux", "arm64"), ("darwin", "arm64"))
MEMBERS = ("registry-cleaner", "README.md", "LICENSE", "BUILD-INFO.txt")
VERSION_RE = re.compile(r"v?(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?\Z")
COMMIT_RE = re.compile(r"[0-9a-f]{40}\Z")
GO_RE = re.compile(r"go[0-9]+\.[0-9]+(?:\.[0-9]+)?(?:[a-z]+[0-9]+)?\Z")


def fail(message):
    raise ValueError(message)


def validate(version, commit, target=None):
    if not VERSION_RE.fullmatch(version):
        fail("invalid release version (expected a safe semantic version tag)")
    if not COMMIT_RE.fullmatch(commit):
        fail("commit must be a full lowercase SHA-1")
    if target is not None and target not in TARGETS:
        fail("unsupported target")


def archive_name(version, goos, goarch):
    return f"docker-registry-cleaner_{version}_{goos}_{goarch}.tar.gz"


def run(*args, cwd=None, env=None):
    return subprocess.check_output(args, cwd=cwd, env=env, text=True).strip()


def build(source, output, version, goos, goarch, commit):
    validate(version, commit, (goos, goarch))
    source, output = Path(source).resolve(strict=True), Path(output).resolve()
    if source == output or source in output.parents:
        fail("output directory must be outside the source checkout")
    if run("git", "rev-parse", "HEAD", cwd=source) != commit:
        fail("source HEAD does not match the expected tag commit")
    if run("git", "status", "--porcelain", "--untracked-files=all", cwd=source):
        fail("source checkout must be clean")
    env = os.environ.copy()
    env.update({"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOFLAGS": "", "GOEXPERIMENT": ""})
    go_version = run("go", "env", "GOVERSION", cwd=source, env=env)
    if not GO_RE.fullmatch(go_version):
        fail("unexpected Go version")
    output.mkdir(parents=True, exist_ok=True)
    info = (f"Version: {version}\nCommit: {commit}\nGo version: {go_version}\n"
            f"GOOS: {goos}\nGOARCH: {goarch}\n").encode()
    with tempfile.TemporaryDirectory() as temp:
        binary = Path(temp) / "registry-cleaner"
        subprocess.run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", str(binary), "./cmd/registry-cleaner"], cwd=source, env=env, check=True)
        contents = (binary.read_bytes(), (source / "README.md").read_bytes(), (source / "LICENSE").read_bytes(), info)
        path = output / archive_name(version, goos, goarch)
        # A fixed gzip timestamp, USTAR fields, modes and ordering make reruns
        # byte-identical when source, target and Go toolchain are unchanged.
        with path.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed, tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as tar:
            for name, data in zip(MEMBERS, contents):
                item = tarfile.TarInfo(name)
                item.size = len(data)
                item.mode = 0o755 if name == "registry-cleaner" else 0o644
                item.uid = item.gid = item.mtime = 0
                item.uname = item.gname = ""
                tar.addfile(item, io.BytesIO(data))
    verify(output, version, goos, goarch, commit)
    print(path)


def verify(output, version, goos, goarch, commit):
    validate(version, commit, (goos, goarch))
    path = Path(output) / archive_name(version, goos, goarch)
    compressed = path.read_bytes()
    decoder = zlib.decompressobj(16 + zlib.MAX_WBITS)
    raw = decoder.decompress(compressed) + decoder.flush()
    if not decoder.eof or decoder.unused_data:
        fail("archive has truncated gzip data or trailing bytes")
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as tar:
        members = tar.getmembers()
        if [m.name for m in members] != list(MEMBERS) or any(not m.isfile() for m in members):
            fail("archive has unexpected entries")
        for member in members:
            if member.mode != (0o755 if member.name == "registry-cleaner" else 0o644):
                fail("archive has unexpected file permissions")
            data = tar.extractfile(member).read()
            if not data:
                fail("archive contains an empty file")
            if member.name == "BUILD-INFO.txt":
                lines = data.decode("utf-8").splitlines()
                if len(lines) != 5 or lines[0] != f"Version: {version}" or lines[1] != f"Commit: {commit}" or not lines[2].startswith("Go version: ") or not GO_RE.fullmatch(lines[2][12:]) or lines[3:] != [f"GOOS: {goos}", f"GOARCH: {goarch}"]:
                    fail("archive build metadata does not match")
    return path


def checksum_lines(output, version, commit):
    return [f"{hashlib.sha256(verify(output, version, goos, goarch, commit).read_bytes()).hexdigest()}  {archive_name(version, goos, goarch)}\n" for goos, goarch in TARGETS]


def main(args):
    if len(args) == 7 and args[0] == "build":
        build(*args[1:])
    elif len(args) == 6 and args[0] == "verify":
        verify(*args[1:])
    elif len(args) == 4 and args[0] in ("checksums", "verify-checksums"):
        command, output, version, commit = args
        validate(version, commit)
        sums = "".join(checksum_lines(output, version, commit))
        path = Path(output) / "SHA256SUMS"
        if command == "checksums":
            path.write_text(sums, encoding="ascii")
        elif path.read_text(encoding="ascii") != sums:
            fail("SHA256SUMS does not match the release archives")
    else:
        fail("usage: release.sh build SOURCE OUTPUT VERSION GOOS GOARCH COMMIT | verify OUTPUT VERSION GOOS GOARCH COMMIT | checksums|verify-checksums OUTPUT VERSION COMMIT")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (ValueError, OSError, UnicodeError, tarfile.TarError, zlib.error, subprocess.CalledProcessError) as exc:
        print(f"release: {exc}", file=sys.stderr)
        sys.exit(1)
