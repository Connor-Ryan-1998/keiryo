from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import release


class VersionTests(unittest.TestCase):
    def test_first_release_and_numeric_increment(self):
        self.assertEqual(release.select_version("0.0.0", {}, "new"), "0.0.1")
        self.assertEqual(
            release.select_version(
                "0.0.0", {"v0.0.9": "old", "v0.0.10": "older", "other": "old"}, "new"
            ),
            "0.0.11",
        )

    def test_manual_major_and_minor_changes(self):
        self.assertEqual(
            release.select_version("1.0.0", {"v0.0.99": "old"}, "new"), "1.0.1"
        )
        self.assertEqual(
            release.select_version("1.1.0", {"v1.0.99": "old"}, "new"), "1.1.1"
        )

    def test_retry_reuses_reserved_tag(self):
        self.assertEqual(
            release.select_version("0.0.0", {"v0.0.1": "same"}, "same"), "0.0.1"
        )

    def test_invalid_or_regressed_series_is_rejected(self):
        for base in ["0.0.1", "01.0.0", "v0.0.0", "0.0", "0.0.0-rc1"]:
            with self.subTest(base=base), self.assertRaises(ValueError):
                release.select_version(base, {}, "new")
        with self.assertRaises(ValueError):
            release.select_version("0.0.0", {"v1.0.1": "old"}, "new")


class PublicationTests(unittest.TestCase):
    def publish(self, *, head="commit", tags=None, released=False, fail_promotion=False):
        self.commands = []
        self.tags = {} if tags is None else tags

        def run(*args):
            self.commands.append(args)
            if args[:2] == ("gh", "api"):
                endpoint = args[2]
                if endpoint.endswith("/git/ref/heads/main"):
                    return head
                if endpoint.endswith("/tags"):
                    return "\n".join(f"{tag}\t{sha}" for tag, sha in self.tags.items())
                if endpoint.endswith("/releases"):
                    return "v0.0.1" if released else ""
                if endpoint.endswith("/git/refs"):
                    tag = args[args.index("-f") + 1].removeprefix("ref=refs/tags/")
                    self.tags[tag] = "commit"
                    return ""
            if args[:3] == ("docker", "buildx", "imagetools"):
                if fail_promotion:
                    raise RuntimeError("registry unavailable")
            return ""

        self.packaged = []

        def package(image, version, out):
            self.packaged.append((image, version))
            return [out / "keiryo.tar.gz", out / "SHA256SUMS"]

        with patch.object(release, "run", side_effect=run), \
                patch.object(release, "package_binaries", side_effect=package):
            release.publish("owner/repo", "commit", "0.0.0")

    def test_stale_commit_does_not_publish(self):
        self.publish(head="newer")
        self.assertEqual(len(self.commands), 1)

    def test_publish_reserves_before_promotion_and_release(self):
        self.publish()
        self.assertEqual(self.tags, {"v0.0.1": "commit"})
        promotion = next(c for c in self.commands if c[0] == "docker")
        self.assertEqual(
            promotion,
            (
                "docker", "buildx", "imagetools", "create",
                "--tag", "ghcr.io/owner/repo:0.0.1",
                "--tag", "ghcr.io/owner/repo:latest",
                "ghcr.io/owner/repo:sha-commit",
            ),
        )
        reserve_index = next(
            i for i, c in enumerate(self.commands)
            if any(arg.endswith("/git/refs") for arg in c)
        )
        promotion_index = self.commands.index(promotion)
        release_index = next(
            i for i, c in enumerate(self.commands) if c[:3] == ("gh", "release", "create")
        )
        self.assertLess(reserve_index, promotion_index)
        self.assertLess(promotion_index, release_index)
        self.assertEqual(self.packaged, [("ghcr.io/owner/repo:sha-commit", "0.0.1")])
        uploaded = [Path(arg).name for arg in self.commands[release_index]]
        self.assertIn("keiryo.tar.gz", uploaded)
        self.assertIn("SHA256SUMS", uploaded)

    def test_failed_packaging_reserves_nothing(self):
        with patch.object(release, "package_binaries", side_effect=RuntimeError("pull failed")), \
                patch.object(release, "run", side_effect=lambda *a: "commit" if a[2].endswith("main") else ""), \
                self.assertRaisesRegex(RuntimeError, "pull failed"):
            release.publish("owner/repo", "commit", "0.0.0")

    def test_completed_release_rerun_does_not_mutate_images(self):
        self.publish(tags={"v0.0.1": "commit"}, released=True)
        self.assertEqual(self.packaged, [])
        self.assertFalse(any(c[0] == "docker" for c in self.commands))
        self.assertFalse(any(c[:3] == ("gh", "release", "create") for c in self.commands))

    def test_failed_promotion_keeps_reservation_and_retry_reuses_it(self):
        tags = {}
        with self.assertRaisesRegex(RuntimeError, "registry unavailable"):
            self.publish(tags=tags, fail_promotion=True)
        self.assertEqual(tags, {"v0.0.1": "commit"})
        self.assertFalse(any(c[:3] == ("gh", "release", "create") for c in self.commands))
        self.publish(tags=tags)
        self.assertEqual(tags, {"v0.0.1": "commit"})
        self.assertFalse(
            any(any(arg.endswith("/git/refs") for arg in c) for c in self.commands)
        )


class PackagingTests(unittest.TestCase):
    def test_archives_contain_both_executables_and_notices(self):
        commands = []

        def run(*args):
            commands.append(args)
            if args[:2] == ("docker", "create"):
                return "cid"
            if args[:2] == ("docker", "cp"):
                source, destination = args[2].split(":", 1)[1], Path(args[3])
                target = destination / Path(source).name
                if source == "/licenses":
                    target.mkdir()
                    (target / "GO-LICENSE").write_text("go")
                else:
                    target.write_text("binary")
            return ""

        with tempfile.TemporaryDirectory() as directory, \
                patch.object(release, "run", side_effect=run):
            assets = release.package_binaries("img:sha-x", "1.2.3", Path(directory))
            names = [asset.name for asset in assets]
            self.assertEqual(names, [
                "keiryo_1.2.3_linux_amd64.tar.gz", "keiryo_1.2.3_linux_arm64.tar.gz", "SHA256SUMS",
            ])
            with tarfile.open(assets[0]) as tar:
                members = set(tar.getnames())
            for member in ["monitoring", "monitoring-agent", "licenses/GO-LICENSE", "LICENSE", "NOTICE"]:
                self.assertIn(f"keiryo_1.2.3_linux_amd64/{member}", members)
            sums = assets[2].read_text().splitlines()
            self.assertEqual([line.split("  ")[1] for line in sums], names[:2])
        self.assertIn(("docker", "create", "--platform", "linux/arm64", "img:sha-x"), commands)
        self.assertEqual(sum(c[:2] == ("docker", "rm") for c in commands), 2)


if __name__ == "__main__":
    unittest.main()
