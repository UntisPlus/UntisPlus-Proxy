#!/usr/bin/env python3
"""Sign in to UntisPlus with a one-time code and request a cancellation report."""

import argparse
from datetime import date, timedelta
from getpass import getpass
from http.cookiejar import CookieJar
import json
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit
from urllib.request import (
    HTTPCookieProcessor,
    HTTPRedirectHandler,
    Request,
    build_opener,
)


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def post(opener, url, payload):
    request = Request(
        url,
        data=json.dumps(payload).encode(),
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
            "User-Agent": "UntisPlus-Analytics-Smoke/1.0",
        },
    )
    with opener.open(request, timeout=120) as response:
        result = json.load(response)
    if not isinstance(result, dict):
        raise ValueError("expected a JSON object")
    return result


def main():
    yesterday = date.today() - timedelta(days=1)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("username", help="UntisPlus account username")
    parser.add_argument("--url", default="https://beta-untis.deelabs.tech")
    parser.add_argument("--school", help="school name if different from the proxy default")
    parser.add_argument("--from", dest="start", type=date.fromisoformat,
                        default=yesterday - timedelta(days=6))
    parser.add_argument("--to", dest="end", type=date.fromisoformat, default=yesterday)
    parser.add_argument("--teacher-id", type=int, help="use an explicitly approved teacher source and target")
    parser.add_argument("--json", action="store_true", help="print the full report")
    parser.add_argument("--show-names", action="store_true", help="include teacher and subject names in the report")
    parser.add_argument("--saved-source", action="store_true",
                        help="use the proxy's saved replay credential for the report")
    args = parser.parse_args()

    url = urlsplit(args.url)
    if url.scheme != "https" or not url.hostname or url.username is not None or url.path not in ("", "/") or url.query or url.fragment:
        parser.error("--url must be the HTTPS origin of your UntisPlus server")
    if not 0 <= (args.end - args.start).days < 366 or args.end > date.today():
        parser.error("choose a date range of up to 366 days ending today or earlier")
    if args.teacher_id is not None and args.teacher_id <= 0:
        parser.error("--teacher-id must be positive")

    origin = args.url.rstrip("/")
    query = "?" + urlencode({"school": args.school}) if args.school else ""
    cookies = CookieJar()
    opener = build_opener(NoRedirect(), HTTPCookieProcessor(cookies))
    if not sys.stdin.isatty():
        parser.error("run in a terminal so the one-time code can be entered privately")
    code = getpass(f"Current WebUntis code for {args.username} at {origin}: ").strip()
    if len(code) != 6 or not code.isascii() or not code.isdecimal():
        parser.error("enter a six-digit authenticator code")

    stage = "Sign-in"
    try:
        login = post(opener, origin + "/WebUntis/jsonrpc_intern.do" + query, {
            "jsonrpc": "2.0", "id": "analytics-smoke", "method": "getUserData2017",
            "params": [{"auth": {"user": args.username, "otp": code,
                                 "clientTime": int(time.time() * 1000)}}],
        })
        if login.get("error") or not any(c.name == "JSESSIONID" for c in cookies):
            print("Sign-in failed. Check the school, username, and current code.", file=sys.stderr)
            return 1

        stage = "Analytics"
        payload = {
            "mode": "personal" if args.teacher_id is None else "teacher",
            "from": args.start.isoformat(), "to": args.end.isoformat(),
            "anonymise": not args.show_names, "format": "json",
        }
        if args.teacher_id is not None:
            payload["teacherId"] = args.teacher_id
        elif not args.saved_source and (args.end - args.start).days < 31:
            payload["personalOtp"] = code
        report = post(opener, origin + "/api/analytics/teacher-cancellations" + query, payload)
    except HTTPError as error:
        detail = error.read(4096)
        if error.code == 403 and (b"Error 1010" in detail or b"error code: 1010" in detail):
            print(f"{stage} was blocked by Cloudflare (error 1010) before reaching UntisPlus.",
                  file=sys.stderr)
            return 1
        print(f"{stage} returned HTTP {error.code}.", file=sys.stderr)
        if stage == "Analytics" and error.code == 403:
            print("Check the analytics feature switch, your permission group, and any approved teacher ID.",
                  file=sys.stderr)
        return 1
    except (URLError, OSError, ValueError):
        print(f"{stage} failed. Check the server URL, network, and response format.", file=sys.stderr)
        return 1

    totals = report.get("teacherAssignmentTotals")
    if not isinstance(totals, dict):
        print("Analytics response was missing report totals.", file=sys.stderr)
        return 1
    if args.json:
        print(json.dumps(report, indent=2, ensure_ascii=False))
    else:
        print(f"{report['source']} | {report['from']} to {report['to']}")
        print(f"{totals['cancelled']} cancelled / {totals['scheduled']} scheduled teacher assignments")
        print(f"{report['uniqueLessons']} unique lessons | {len(report.get('teachers') or [])} teachers")
        if "lessonChanges" in report:
            print(f"{report['cancelledLessons']} cancelled lessons | "
                  f"{report['coveredLessons']} covered lessons | "
                  f"{report['clashingLessons']} observed overlaps")
            for key, title in (("missedTeachers", "Most missed teacher"),
                               ("missedSubjects", "Most missed subject"),
                               ("coverTeachers", "Most frequent cover teacher")):
                if rows := report.get(key):
                    leader = rows[0]
                    print(f"{title}: {leader['name']} ({leader['lessons']} lessons)")
        for warning in report.get("warnings") or []:
            print(f"Warning: {warning}")
    if not report.get("complete") or not any(report.get(key) for key in
                                             ("uniqueLessons", "cancelledLessons", "coveredLessons")):
        if payload.get("personalOtp"):
            print("The live-code report is partial or empty. Use --saved-source to compare the saved credential.",
                  file=sys.stderr)
        else:
            print("The saved-source report is partial or empty. Check account enrolment and the requested dates.",
                  file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
