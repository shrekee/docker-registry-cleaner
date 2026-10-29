#!/usr/bin/env python3
"""Black-box checks for release packaging; requires git, Python 3 and Go.

Uses an isolated synthetic source repository, not the working tree or the network.
Run from any directory: python3 tests/test_release.py
"""
import hashlib
import os
from pathlib import Path
import subprocess
import platform
import struct
import tarfile
import tempfile
import unittest

TOOL = Path(__file__).resolve().parents[1] / 'scripts' / 'release.sh'


def command(*args, **kwargs):
    return subprocess.run(args, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, **kwargs)


class ReleaseHelperTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temp.name)
        cls.old_gocache = os.environ.get('GOCACHE')
        os.environ['GOCACHE'] = str(cls.root / 'go-cache')
        cls.source = cls.root / 'source'
        cls.output = cls.root / 'output'
        cls.source.mkdir()
        (cls.source / 'cmd' / 'registry-cleaner').mkdir(parents=True)
        (cls.source / 'go.mod').write_text('module example.test/cleaner\n\ngo 1.26.0\n')
        (cls.source / 'cmd' / 'registry-cleaner' / 'main.go').write_text(
            'package main\nimport "fmt"\nfunc main() { fmt.Println("fixture") }\n')
        (cls.source / 'README.md').write_text('fixture README\n')
        (cls.source / 'LICENSE').write_text('fixture LICENSE\n')
        for args in [('init', '-q'), ('config', 'user.email', 'release-test@example.test'),
                     ('config', 'user.name', 'Release Test'), ('add', '.'),
                     ('commit', '-qm', 'fixture')]:
            subprocess.run(('git', '-C', str(cls.source), *args), check=True)
        cls.commit = subprocess.check_output(('git', '-C', str(cls.source), 'rev-parse', 'HEAD'), text=True).strip()
        cls.version = '0.1.0'
        cls.go_version = subprocess.check_output(('go', 'version'), text=True).strip().split()[2]
        for target in (('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'arm64')):
            result = cls.invoke('build', cls.source, cls.output, cls.version, *target, cls.commit)
            if result.returncode:
                raise RuntimeError(f'fixture build {target} failed:\n' + result.stdout + result.stderr)
        result = cls.invoke('checksums', cls.output, cls.version, cls.commit)
        if result.returncode:
            raise RuntimeError('checksum generation failed:\n' + result.stdout + result.stderr)

    @classmethod
    def tearDownClass(cls):
        if cls.old_gocache is None:
            os.environ.pop('GOCACHE', None)
        else:
            os.environ['GOCACHE'] = cls.old_gocache
        cls.temp.cleanup()

    @staticmethod
    def invoke(*args):
        return command('bash', str(TOOL), *(str(x) for x in args))

    def test_archive_has_expected_files_and_metadata(self):
        path = self.output / f'docker-registry-cleaner_{self.version}_linux_amd64.tar.gz'
        with tarfile.open(path, 'r:gz') as archive:
            names = archive.getnames()
            self.assertEqual(set(names), {'registry-cleaner', 'README.md', 'LICENSE', 'BUILD-INFO.txt'})
            self.assertEqual(len(names), len(set(names)))
            self.assertTrue(all(member.isfile() for member in archive.getmembers()))
            self.assertEqual(archive.extractfile('README.md').read(), b'fixture README\n')
            self.assertEqual(archive.extractfile('LICENSE').read(), b'fixture LICENSE\n')
            metadata = archive.extractfile('BUILD-INFO.txt').read().decode()
            self.assertIn(self.commit, metadata)
            self.assertIn(self.go_version, metadata)
            self.assertIn(self.version, metadata)
            self.assertIn('linux', metadata)
            self.assertIn('amd64', metadata)
            binary = archive.extractfile('registry-cleaner').read()
            self.assertEqual(binary[:4], b'\x7fELF')
        verified = self.invoke('verify', self.output, self.version, 'linux', 'amd64', self.commit)
        self.assertEqual(verified.returncode, 0, verified.stdout + verified.stderr)

    @unittest.skipUnless(platform.system() == 'Linux' and platform.machine() in ('x86_64', 'AMD64'),
                         'Linux/amd64 fixture execution requires a native Linux/amd64 host')
    def test_linux_amd64_archive_binary_executes_natively(self):
        path = self.output / f'docker-registry-cleaner_{self.version}_linux_amd64.tar.gz'
        extracted = self.root / 'native-test-binary'
        with tarfile.open(path, 'r:gz') as archive:
            extracted.write_bytes(archive.extractfile('registry-cleaner').read())
        extracted.chmod(0o755)
        result = command(str(extracted))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'fixture\n')

    def test_all_targets_and_checksums(self):
        sums = self.output / 'SHA256SUMS'
        lines = sums.read_text().splitlines()
        self.assertEqual(len(lines), 3)
        seen = set()
        for line in lines:
            digest, filename = line.split(maxsplit=1)
            filename = filename.lstrip('*')
            self.assertNotIn(filename, seen)
            seen.add(filename)
            path = self.output / filename
            self.assertTrue(path.is_file())
            self.assertEqual(digest, hashlib.sha256(path.read_bytes()).hexdigest())
        self.assertEqual(seen, {
            f'docker-registry-cleaner_{self.version}_{os_name}_{arch}.tar.gz'
            for os_name, arch in (('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'arm64'))
        })
        verified = self.invoke('verify-checksums', self.output, self.version, self.commit)
        self.assertEqual(verified.returncode, 0, verified.stdout + verified.stderr)
        # Cross-compiled target formats are checked without claiming they ran natively.
        for os_name, arch, magic in (('linux', 'arm64', b'\x7fELF'),
                                      ('darwin', 'arm64', b'\xcf\xfa\xed\xfe')):
            path = self.output / f'docker-registry-cleaner_{self.version}_{os_name}_{arch}.tar.gz'
            with tarfile.open(path, 'r:gz') as archive:
                header = archive.extractfile('registry-cleaner').read(64)
                self.assertEqual(header[:4], magic)
                if os_name == 'linux':
                    self.assertEqual(struct.unpack_from('<H', header, 18)[0], 183)  # ELF AArch64
                else:
                    self.assertEqual(struct.unpack_from('<I', header, 4)[0], 0x0100000c)  # Mach-O arm64
                self.assertIn(self.commit, archive.extractfile('BUILD-INFO.txt').read().decode())
            verified = self.invoke('verify', self.output, self.version, os_name, arch, self.commit)
            self.assertEqual(verified.returncode, 0, verified.stdout + verified.stderr)

    def test_modified_checksum_manifest_rejected(self):
        import shutil
        altered = self.root / 'altered-sums'
        shutil.copytree(self.output, altered)
        sums = altered / 'SHA256SUMS'
        sums.write_text(sums.read_text().replace('a', 'b', 1))
        result = self.invoke('verify-checksums', altered, self.version, self.commit)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_unsafe_versions_rejected(self):
        # Metacharacters, traversal, whitespace and controls must never reach paths or shell.
        for version in ('../0.1.0', '0.1.0/../../oops', '0.1.0;touch PWNED',
                        '0.1.0$(touch PWNED)', '0.1.0\nnext', '0.1.0+build', '-1.2.3', ''):
            with self.subTest(version=version):
                result = self.invoke('build', self.source, self.root / 'bad-output', version,
                                     'linux', 'amd64', self.commit)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.root / 'PWNED').exists())
        self.assertFalse((self.source / 'PWNED').exists())

    def test_dirty_source_and_unsupported_target_rejected(self):
        readme = self.source / 'README.md'
        original = readme.read_bytes()
        try:
            readme.write_bytes(b'dirty source')
            result = self.invoke('build', self.source, self.root / 'dirty', self.version,
                                 'linux', 'amd64', self.commit)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        finally:
            readme.write_bytes(original)
        result = self.invoke('build', self.source, self.root / 'bad-target', self.version,
                             'linux', 'amd64;touch PWNED', self.commit)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.source / 'PWNED').exists())

    def test_rebuild_is_byte_identical(self):
        rebuilt = self.root / 'rebuilt'
        result = self.invoke('build', self.source, rebuilt, self.version,
                             'linux', 'amd64', self.commit)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        filename = f'docker-registry-cleaner_{self.version}_linux_amd64.tar.gz'
        self.assertEqual((rebuilt / filename).read_bytes(), (self.output / filename).read_bytes())

    def test_wrong_commit_rejected(self):
        result = self.invoke('build', self.source, self.root / 'wrong-commit', self.version,
                             'linux', 'amd64', '0' * 40)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_archive_tampering_detected(self):
        # Work on a copy, so other tests retain the frozen original.
        import shutil
        tampered = self.root / 'tampered'
        shutil.copytree(self.output, tampered)
        path = tampered / f'docker-registry-cleaner_{self.version}_linux_amd64.tar.gz'
        with path.open('ab') as out:
            out.write(b'tampering')
        result = self.invoke('verify', tampered, self.version, 'linux', 'amd64', self.commit)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

if __name__ == '__main__':
    unittest.main(verbosity=2)
