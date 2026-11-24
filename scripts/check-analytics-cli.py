#!/usr/bin/env python3
import contextlib
import importlib.util
import io
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from urllib.error import HTTPError


spec = importlib.util.spec_from_file_location(
    "analytics_cli", Path(__file__).with_name("test-teacher-cancellations.py")
)
cli = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cli)


class AnalyticsCLITest(unittest.TestCase):
    def test_room_uses_saved_teacher_source_even_for_a_short_range(self):
        argv = ["analytics", "teacher", "--room", "14-05", "--from", "2026-09-21",
                "--to", "2026-09-27", "--json", "--show-names"]
        report = {"source": "room", "complete": True, "uniqueLessons": 2,
                  "teacherAssignmentTotals": {"scheduled": 2, "cancelled": 1}}
        output = io.StringIO()
        with patch.object(cli.sys, "argv", argv), patch.object(cli.sys.stdin, "isatty", return_value=True), \
                patch.object(cli, "getpass", return_value="123456"), \
                patch.object(cli, "CookieJar", return_value=[SimpleNamespace(name="JSESSIONID")]), \
                patch.object(cli, "post", side_effect=[{"result": {}}, report]) as post, \
                contextlib.redirect_stdout(output):
            self.assertEqual(cli.main(), 0)

        self.assertEqual(post.call_args.args[2], {
            "mode": "room", "room": "14-05", "from": "2026-09-21", "to": "2026-09-27",
            "anonymise": False, "format": "json",
        })
        self.assertEqual(json.loads(output.getvalue()), report)

    def test_multiple_years_and_teachers_keep_individual_coverage(self):
        argv = ["analytics", "caller", "--teacher-id", "42", "--teacher-id", "43",
                "--school-year", "2024/2025", "--school-year", "2025/2026", "--json"]
        stub = {"complete": True, "teacherAssignmentTotals": {"scheduled": 3, "cancelled": 1}}
        forbidden = HTTPError("https://example.test", 403, "Forbidden", {}, io.BytesIO())
        output = io.StringIO()
        with patch.object(cli.sys, "argv", argv), patch.object(cli.sys.stdin, "isatty", return_value=True), \
                patch.object(cli, "getpass", return_value="123456"), \
                patch.object(cli, "CookieJar", return_value=[SimpleNamespace(name="JSESSIONID")]), \
                patch.object(cli, "post", side_effect=[{"result": {}}, stub, stub, forbidden, stub]) as post, \
                contextlib.redirect_stdout(output):
            self.assertEqual(cli.main(), 2)

        requests = [call.args[2] for call in post.call_args_list[1:]]
        self.assertEqual([(p["teacherId"], p["schoolYear"]) for p in requests], [
            (42, "2024/2025"), (42, "2025/2026"), (43, "2024/2025"), (43, "2025/2026"),
        ])
        self.assertTrue(all("from" not in p and "to" not in p and "personalOtp" not in p for p in requests))
        comparison = json.loads(output.getvalue())
        self.assertFalse(comparison["complete"])
        self.assertEqual(comparison["reports"][2], {
            "schoolYear": "2024/2025", "teacherSelection": 2, "httpStatus": 403,
        })
        self.assertEqual(comparison["reports"][3]["report"], stub)
        self.assertNotIn("teacherId", output.getvalue())


if __name__ == "__main__":
    unittest.main()
