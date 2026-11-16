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

        with patch.object(release, "run", side_effect=run):
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

    def test_completed_release_rerun_does_not_mutate_images(self):
        self.publish(tags={"v0.0.1": "commit"}, released=True)
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


if __name__ == "__main__":
    unittest.main()
