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
    return read_json(opener, request)


def read_json(opener, request):
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
    parser.add_argument("--student-id", type=int, action="append", help="select an approved student ID")
    parser.add_argument("--class-id", type=int, action="append", help="select your class or an approved class ID")
    parser.add_argument("--room", help="export a room by exact name or ID")
    parser.add_argument("--data-source", choices=("personal", "own-teacher", "saved-teacher", "shared-class", "reconstruction"))
    parser.add_argument("--options", action="store_true", help="list accessible targets, sources and school years")
    parser.add_argument("--json", action="store_true", help="print the full report")
    parser.add_argument("--show-names", action="store_true", help="include teacher and subject names in the report")
    parser.add_argument("--saved-source", action="store_true",
                        help="use the proxy's saved replay credential for the report")
    args = parser.parse_args()

    url = urlsplit(args.url)
    if url.scheme != "https" or not url.hostname or url.username is not None or url.path not in ("", "/") or url.query or url.fragment:
        parser.error("--url must be the HTTPS origin of your UntisPlus server")
    years = list(dict.fromkeys(args.school_year or [None]))
    teachers = list(dict.fromkeys(args.teacher_id or []))
    if args.school_year:
        if len(years) > 1 and (args.start is not None or args.end is not None):
            parser.error("use explicit dates with a single school year")
        if args.start and args.end and args.end < args.start:
            parser.error("the end date must not come before the start date")
    else:
        args.end = args.end or yesterday
        args.start = args.start or (yesterday - timedelta(days=6))
        if not 0 <= (args.end - args.start).days < 366 or args.end > date.today():
            parser.error("choose a date range of up to 366 days ending today or earlier")
    if any(teacher <= 0 for teacher in teachers):
        parser.error("--teacher-id must be positive")
    targets = [("teacher", teacher) for teacher in teachers]
    for mode, ids in (("student", args.student_id), ("class", args.class_id)):
        if ids and any(target <= 0 for target in ids):
            parser.error(f"--{mode}-id must be positive")
        targets.extend((mode, target) for target in dict.fromkeys(ids or []))
    if args.room is not None:
        if not args.room.strip():
            parser.error("--room requires a non-empty room name or ID")
        targets.append(("room", args.room.strip()))
    if not targets:
        targets.append(("personal", None))
    count = len(years) * len(targets)
    if count > 6:
        parser.error("select at most six target and school year combinations")

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
        if args.options:
            url = origin + "/api/analytics/options" + query
            options = read_json(opener, Request(url, headers={
                "Accept": "application/json", "User-Agent": "UntisPlus-Analytics-Smoke/1.0",
            }))
            print(json.dumps(options, indent=2, ensure_ascii=False))
            return 0
        reports = []
        for index, (mode, target) in enumerate(targets, start=1):
            for year in years:
                payload = {"mode": mode,
                           "anonymise": not args.show_names, "format": "json"}
                if year is not None:
                    payload["schoolYear"] = year
                    if args.start:
                        payload["from"] = args.start.isoformat()
                    if args.end:
                        payload["to"] = args.end.isoformat()
                else:
                    payload.update({"from": args.start.isoformat(), "to": args.end.isoformat()})
                if args.data_source is not None:
                    payload["dataSource"] = args.data_source
                if mode in ("teacher", "student", "class"):
                    payload[f"{mode}Id"] = target
                elif mode == "room":
                    payload["room"] = target
                elif year is None and not args.saved_source and (args.end - args.start).days < 31:
                    payload["personalOtp"] = code
                entry = {"schoolYear": year}
                if mode in ("teacher", "student", "class"):
                    entry[f"{mode}Id" if args.show_names else f"{mode}Selection"] = target if args.show_names else index
                elif mode == "room":
                    entry["room" if args.show_names else "roomSelection"] = target if args.show_names else 1
                try:
                    entry["report"] = post(opener, origin + "/api/analytics/teacher-cancellations" + query, payload)
                except HTTPError as error:
                    if count == 1:
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
                if "teacherId" in entry or "teacherSelection" in entry:
                    label = f"Teacher {entry.get('teacherId', entry.get('teacherSelection'))}"
                elif "studentId" in entry or "studentSelection" in entry:
                    label = f"Student {entry.get('studentId', entry.get('studentSelection'))}"
                elif "classId" in entry or "classSelection" in entry:
                    label = f"Class {entry.get('classId', entry.get('classSelection'))}"
                elif "room" in entry or "roomSelection" in entry:
                    label = f"Room {entry.get('room', entry.get('roomSelection'))}"
                else:
                    label = "Personal"
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
