import unittest

from resolve_release_version import next_release_version, parse_version


def release(tag, *, draft=False, prerelease=False):
    return {"tagName": tag, "isDraft": draft, "isPrerelease": prerelease}


class ReleaseVersionTests(unittest.TestCase):
    def test_empty_releases_start_at_001(self):
        self.assertEqual(next_release_version([]), "0.0.1")

    def test_patch_is_incremented_only(self):
        self.assertEqual(next_release_version([release("0.0.1")]), "0.0.2")
        self.assertEqual(next_release_version([release("0.9.99")]), "0.9.100")
        self.assertEqual(next_release_version([release("1.3.7")]), "1.3.8")

    def test_numeric_semver_max_not_lexicographic_or_recent_list_order(self):
        tags = [release("1.2.10"), release("2.0.1"), release("1.8.99"), release("0.99.1")]
        self.assertEqual(next_release_version(tags), "2.0.2")

    def test_legacy_v_prefix_is_supported(self):
        self.assertEqual(next_release_version([release("v0.0.3")]), "0.0.4")
        self.assertEqual(next_release_version([release("v12.3.9")]), "12.3.10")

    def test_draft_prerelease_and_unrelated_tags_are_ignored(self):
        tags = [
            release("2.0.1", draft=True),
            release("v0.9.5", prerelease=True),
            release("nightly"),
            release("0.0.7"),
            release("0.0.6"),
        ]
        self.assertEqual(next_release_version(tags), "0.0.8")
        self.assertEqual(next_release_version([release("0.0.3", draft=True)]), "0.0.1")

    def test_explicit_version_overrides_auto(self):
        self.assertEqual(next_release_version([release("2.0.2")], "0.0.5"), "0.0.5")
        self.assertEqual(next_release_version([], "v3.1.4"), "v3.1.4")

    def test_invalid_explicit_versions_rejected(self):
        for tag in ("a.b.c", "1.2", "1.2.3.4", "1.2.-1", "latest", "01.2.3", "1.2.3 \nfoo"):
            with self.subTest(tag=tag):
                with self.assertRaises(ValueError):
                    next_release_version([], tag)

    def test_invalid_release_schema_ignored(self):
        self.assertEqual(next_release_version([None, "v1.0.0", {"tagName": "0.1.0"}]), "0.1.1")


if __name__ == "__main__":
    unittest.main()
