"""Exercise the remote helper in a temporary filesystem; no SSH or root access."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class PoolPermissionsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.pool = root / "pool"
        self.pool.mkdir(mode=0o700)
        self.binary = self.pool / "pgbouncer-1.26.1"
        self.binary.write_text("binary fixture")
        self.binary.chmod(0o755)
        source = Path(__file__).with_name("permissions.sh").read_text()
        self.script = source.replace("/usr/local/lib/isureal", str(self.pool))
        # Replace sudo with a local adapter. GNU stat is emulated for macOS too.
        sudo = root / "sudo"
        sudo.write_text(
            "#!/usr/bin/env python3\n"
            "import os, stat, subprocess, sys\n"
            "args = sys.argv[1:]\n"
            "if args[:3] == ['stat', '-c', '%a']:\n"
            "    print(format(stat.S_IMODE(os.stat(args[3]).st_mode), 'o'))\n"
            "else:\n"
            "    sys.exit(subprocess.call(args))\n"
        )
        sudo.chmod(0o755)
        self.env = dict(os.environ, PATH=f"{root}:{os.environ['PATH']}")

    def run_helper(self, dry=False):
        return subprocess.run(
            ["bash", "-s", "--", "1" if dry else "0"],
            input=self.script, text=True, capture_output=True, env=self.env,
        )

    def test_repairs_existing_0700_directory_and_requests_restart_once(self):
        result = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("usr/local/lib/isureal/pgbouncer-1.26.1", result.stdout)
        self.assertEqual(self.pool.stat().st_mode & 0o777, 0o755)
        again = self.run_helper()
        self.assertEqual(again.returncode, 0, again.stderr)
        self.assertEqual(again.stdout, "")

    def test_repairs_missing_binary_execute_bit(self):
        self.pool.chmod(0o755)
        self.binary.chmod(0o644)
        result = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout)
        self.assertEqual(self.binary.stat().st_mode & 0o777, 0o755)

    def test_dry_run_reports_without_changing_modes(self):
        result = self.run_helper(dry=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout)
        self.assertEqual(self.pool.stat().st_mode & 0o777, 0o700)

    def test_missing_binary_fails_deploy_but_allows_first_dry_run(self):
        self.binary.unlink()
        self.assertNotEqual(self.run_helper().returncode, 0)
        result = self.run_helper(dry=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
