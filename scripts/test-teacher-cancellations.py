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
    parser.add_argument("--from", dest="start", type=date.fromisoformat)
    parser.add_argument("--to", dest="end", type=date.fromisoformat)
    parser.add_argument("--school-year", action="append", help="select an imported school year, repeat to compare years")
    parser.add_argument("--teacher-id", type=int, action="append", help="select an approved teacher ID, repeat to compare teachers")
    parser.add_argument("--room", help="export a room by exact name or ID using your own saved teacher account")
    parser.add_argument("--json", action="store_true", help="print the full report")
    parser.add_argument("--show-names", action="store_true", help="include teacher and subject names in the report")
    parser.add_argument("--saved-source", action="store_true",
                        help="use the proxy's saved replay credential for the report")
    args = parser.parse_args()

    url = urlsplit(args.url)
    if url.scheme != "https" or not url.hostname or url.username is not None or url.path not in ("", "/") or url.query or url.fragment:
        parser.error("--url must be the HTTPS origin of your UntisPlus server")
    years = list(dict.fromkeys(args.school_year or [None]))
    teachers = list(dict.fromkeys(args.teacher_id or [None]))
    if args.school_year:
        if args.start is not None or args.end is not None:
            parser.error("select school years or explicit dates")
    else:
        args.end = args.end or yesterday
        args.start = args.start or (yesterday - timedelta(days=6))
        if not 0 <= (args.end - args.start).days < 366 or args.end > date.today():
            parser.error("choose a date range of up to 366 days ending today or earlier")
    if any(teacher is not None and teacher <= 0 for teacher in teachers):
        parser.error("--teacher-id must be positive")
    if args.room is not None and (not args.room.strip() or args.teacher_id):
        parser.error("--room requires a non-empty room name or ID and cannot be combined with --teacher-id")
    if len(years) * len(teachers) > 6:
        parser.error("select at most six teacher and school year combinations")

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
        reports = []
        for index, teacher in enumerate(teachers, start=1):
            for year in years:
                payload = {"mode": "personal" if teacher is None else "teacher",
                           "anonymise": not args.show_names, "format": "json"}
                if args.room is not None:
                    payload.update({"mode": "room", "room": args.room.strip()})
                if year is not None:
                    payload["schoolYear"] = year
                else:
                    payload.update({"from": args.start.isoformat(), "to": args.end.isoformat()})
                if teacher is not None:
                    payload["teacherId"] = teacher
                elif args.room is None and year is None and not args.saved_source and (args.end - args.start).days < 31:
                    payload["personalOtp"] = code
                entry = {"schoolYear": year}
                if teacher is not None:
                    entry["teacherId" if args.show_names else "teacherSelection"] = teacher if args.show_names else index
                try:
                    entry["report"] = post(opener, origin + "/api/analytics/teacher-cancellations" + query, payload)
                except HTTPError as error:
                    if len(teachers) * len(years) == 1:
                        raise
                    entry["httpStatus"] = error.code
                    error.close()
                reports.append(entry)
    except HTTPError as error:
        detail = error.read(4096)
        if error.code == 403 and (b"Error 1010" in detail or b"error code: 1010" in detail):
            print(f"{stage} was blocked by Cloudflare (error 1010) before reaching UntisPlus.",
                  file=sys.stderr)
            return 1
        print(f"{stage} returned HTTP {error.code}.", file=sys.stderr)
        if stage == "Analytics" and error.code == 403:
            try:
                error_body = json.loads(detail)
            except (ValueError, UnicodeError):
                error_body = {}
            reason = error_body.get("error") if isinstance(error_body, dict) else None
            print(reason if isinstance(reason, str) and reason != "forbidden" else
                  "Check the analytics switch, your permission group, and the selected teacher or room.",
                  file=sys.stderr)
        return 1
    except (URLError, OSError, ValueError):
        print(f"{stage} failed. Check the server URL, network, and response format.", file=sys.stderr)
        return 1

    if len(reports) > 1:
        complete = all(entry.get("report", {}).get("complete") is True for entry in reports)
        if args.json:
            print(json.dumps({"complete": complete, "reports": reports}, indent=2, ensure_ascii=False))
        else:
            for entry in reports:
                label = f"Teacher {entry.get('teacherId', entry.get('teacherSelection'))}" if args.teacher_id else "Room" if args.room else "Personal"
                report = entry.get("report")
                date_label = entry["schoolYear"] or f"{args.start} to {args.end}"
                if report is None:
                    print(f"{label} | {date_label} | HTTP {entry['httpStatus']}")
                else:
                    totals = report.get("teacherAssignmentTotals") or {}
                    coverage = "complete" if report.get("complete") else "partial"
                    print(f"{label} | {date_label} | {totals.get('cancelled', '?')} cancelled / {totals.get('scheduled', '?')} scheduled | {coverage}")
        return 0 if complete else 2

    report = reports[0]["report"]
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
