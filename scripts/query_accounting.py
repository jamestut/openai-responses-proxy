#!/usr/bin/env python3
"""Print a readable summary from the Responses proxy accounting endpoint."""

import argparse
from datetime import datetime, timezone
import json
import os
import sys
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


API_KEY_ENV = "RESPONSES_PROXY_API_KEY"
SUMMARY_FIELDS = (
    "request_count",
    "input_tokens",
    "output_tokens",
    "total_tokens",
    "cached_tokens",
    "cache_write_tokens",
    "reasoning_tokens",
    "pending_count",
    "incomplete_count",
)


class SafeArgumentParser(argparse.ArgumentParser):
    def error(self, _message):
        self.print_usage(sys.stderr)
        self.exit(2, "query_accounting.py: invalid command-line arguments\n")


class NoRedirectHandler(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def parse_args():
    parser = SafeArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument("base_url", nargs="?", default="http://127.0.0.1:8080", metavar="BASE_URL", help="proxy root URL")
    parser.add_argument("-g", "--group-by", choices=("model", "user", "model,user"), metavar="GROUP_BY", help="model, user, or model,user")
    token_format = parser.add_mutually_exclusive_group()
    token_format.add_argument("--human", action="store_true", help="format token counts with k/M/B/T suffixes")
    token_format.add_argument("--raw", action="store_true", help="print token counts without separators")
    parser.add_argument("-m", "--model")
    parser.add_argument("-u", "--user")
    period = parser.add_mutually_exclusive_group()
    period.add_argument("-p", "--period", help="YYYY, YYYY-MM, or YYYY-MM-DD")
    period.add_argument("-t", "--today", action="store_true", help="query the current UTC date")
    parser.add_argument("-s", "--start", help="inclusive date or RFC3339 boundary")
    parser.add_argument("-e", "--end", help="exclusive date or RFC3339 boundary")
    parser.add_argument("-z", "--timezone", help="IANA timezone; defaults to UTC")
    args = parser.parse_args()
    if args.today:
        args.period = datetime.now(timezone.utc).date().isoformat()
    return args


def endpoint_url(base_url, params):
    try:
        parsed = urlsplit(base_url)
        hostname = parsed.hostname
        parsed.port
    except ValueError:
        raise ValueError("base URL must be an HTTP(S) proxy root") from None
    if (
        parsed.scheme not in ("http", "https")
        or not hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("", "/")
        or parsed.query
        or parsed.fragment
    ):
        raise ValueError("base URL must be an HTTP(S) proxy root")
    query = urlencode(params)
    suffix = f"?{query}" if query else ""
    return f"{parsed.scheme}://{parsed.netloc}/accounting{suffix}"


def request_report(url, api_key):
    try:
        request = Request(url, headers={"Authorization": f"Bearer {api_key}", "Accept": "application/json"})
    except (TypeError, ValueError):
        raise RuntimeError("could not prepare the proxy request") from None
    opener = build_opener(NoRedirectHandler())
    try:
        with opener.open(request, timeout=30) as response:
            body = response.read()
    except HTTPError as exc:
        if exc.code == 401:
            raise RuntimeError("HTTP 401: check RESPONSES_PROXY_API_KEY") from None
        if 300 <= exc.code < 400:
            raise RuntimeError(f"HTTP {exc.code}: proxy redirects are not followed") from None
        raise RuntimeError(f"HTTP {exc.code} from the proxy") from None
    except (URLError, TimeoutError, OSError):
        raise RuntimeError("could not reach the proxy (check its URL, network, TLS, or timeout)") from None

    try:
        return json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise RuntimeError("proxy returned invalid JSON") from None


def summary_values(summary):
    if not isinstance(summary, dict) or any(type(summary.get(key)) is not int for key in SUMMARY_FIELDS):
        raise ValueError("proxy returned an invalid accounting summary")
    return summary


def group_value(group, key):
    value = group.get(key)
    if value is None or value == "":
        return "(unspecified)"
    if not isinstance(value, str):
        raise ValueError("proxy returned an invalid accounting group")
    return value


def format_token_count(value, token_format):
    if token_format == "raw":
        return str(value)
    if token_format == "comma":
        return f"{value:,}"

    magnitude = abs(value)
    if magnitude >= 1_000_000_000_000_000:
        return f"{value:,}"
    if magnitude < 1_000:
        return str(value)

    units = ((1_000, "k"), (1_000_000, "M"), (1_000_000_000, "B"), (1_000_000_000_000, "T"))
    unit_index = max(index for index, (divisor, _) in enumerate(units) if magnitude >= divisor)
    divisor, suffix = units[unit_index]
    number = f"{magnitude / divisor:.1f}"
    if number.endswith(".0"):
        number = number[:-2]
    if number == "1000" and unit_index < len(units) - 1:
        divisor, suffix = units[unit_index + 1]
        number = f"{magnitude / divisor:.1f}"
        if number.endswith(".0"):
            number = number[:-2]
    sign = "-" if value < 0 else ""
    return f"{sign}{number} {suffix}"


def render_summary(summary, indent, token_format):
    values = summary_values(summary)
    lines = [
        f"{indent}- Requests: {values['request_count']}",
        f"{indent}- Tokens:",
        f"{indent}  - Input: {format_token_count(values['input_tokens'], token_format)}",
        f"{indent}    - Cached: {format_token_count(values['cached_tokens'], token_format)}",
        f"{indent}    - Written to cache: {format_token_count(values['cache_write_tokens'], token_format)}",
        f"{indent}  - Output: {format_token_count(values['output_tokens'], token_format)}",
        f"{indent}    - Reasoning: {format_token_count(values['reasoning_tokens'], token_format)}",
        f"{indent}  - Total: {format_token_count(values['total_tokens'], token_format)}",
        f"{indent}- Pending requests: {values['pending_count']}",
        f"{indent}- Incomplete requests: {values['incomplete_count']}",
    ]
    return lines


def render_report(report, group_by, token_format):
    if not isinstance(report, dict):
        raise ValueError("proxy returned an invalid accounting response")
    lines = ["Accounting summary"]
    if group_by is None:
        lines.extend(render_summary(report.get("totals"), "", token_format))
        return lines

    groups = [] if report == {} else report.get("groups")
    if not isinstance(groups, list):
        raise ValueError("proxy returned an invalid accounting response")
    if not groups:
        lines.append("No matching records.")
        return lines
    if any(not isinstance(group, dict) for group in groups):
        raise ValueError("proxy returned an invalid accounting group")

    if group_by == "user":
        groups.sort(key=lambda item: group_value(item, "user"))
        for group in groups:
            lines.append(f"- User: {group_value(group, 'user')}")
            lines.extend(render_summary(group, "  ", token_format))
    elif group_by == "model":
        groups.sort(key=lambda item: group_value(item, "model"))
        for group in groups:
            lines.append(f"- Model: {group_value(group, 'model')}")
            lines.extend(render_summary(group, "  ", token_format))
    else:
        groups.sort(key=lambda item: (group_value(item, "user"), group_value(item, "model")))
        current_user = None
        for group in groups:
            user = group_value(group, "user")
            if user != current_user:
                lines.append(f"- User: {user}")
                current_user = user
            lines.append(f"  - Model: {group_value(group, 'model')}")
            lines.extend(render_summary(group, "    ", token_format))
    return lines


def main():
    args = parse_args()
    api_key = os.environ.get(API_KEY_ENV)
    if not api_key:
        print(f"query_accounting.py: set {API_KEY_ENV} to a valid user key", file=sys.stderr)
        return 2
    if "\r" in api_key or "\n" in api_key:
        print(f"query_accounting.py: {API_KEY_ENV} contains invalid characters", file=sys.stderr)
        return 2

    params = []
    for name in ("model", "user", "group_by", "period", "start", "end", "timezone"):
        value = getattr(args, name)
        if value is not None:
            params.append((name, value))
    try:
        url = endpoint_url(args.base_url, params)
        report = request_report(url, api_key)
        token_format = "human" if args.human else "raw" if args.raw else "comma"
        lines = render_report(report, args.group_by, token_format)
    except (ValueError, RuntimeError) as exc:
        print(f"query_accounting.py: {exc}", file=sys.stderr)
        return 1

    print("\n".join(lines))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
