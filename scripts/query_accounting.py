#!/usr/bin/env python3
"""Print a readable summary from the Responses proxy accounting endpoint."""

import argparse
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
    parser.add_argument("--base-url", default="http://127.0.0.1:8080", help="proxy root URL")
    parser.add_argument("--group-by", choices=("model", "user", "model,user"), metavar="GROUP_BY", help="model, user, or model,user")
    parser.add_argument("--model")
    parser.add_argument("--user")
    parser.add_argument("--period", help="YYYY, YYYY-MM, or YYYY-MM-DD")
    parser.add_argument("--start", help="inclusive date or RFC3339 boundary")
    parser.add_argument("--end", help="exclusive date or RFC3339 boundary")
    parser.add_argument("--timezone", help="IANA timezone; defaults to UTC")
    return parser.parse_args()


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


def render_summary(summary, indent):
    values = summary_values(summary)
    lines = [
        f"{indent}- Requests: {values['request_count']}",
        f"{indent}- Tokens:",
        f"{indent}  - Input: {values['input_tokens']}",
        f"{indent}    - Cached: {values['cached_tokens']}",
        f"{indent}    - Written to cache: {values['cache_write_tokens']}",
        f"{indent}  - Output: {values['output_tokens']}",
        f"{indent}    - Reasoning: {values['reasoning_tokens']}",
        f"{indent}  - Total: {values['total_tokens']}",
        f"{indent}- Pending requests: {values['pending_count']}",
        f"{indent}- Incomplete requests: {values['incomplete_count']}",
    ]
    return lines


def render_report(report, group_by):
    if not isinstance(report, dict):
        raise ValueError("proxy returned an invalid accounting response")
    lines = ["Accounting summary"]
    if group_by is None:
        lines.extend(render_summary(report.get("totals"), ""))
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
            lines.extend(render_summary(group, "  "))
    elif group_by == "model":
        groups.sort(key=lambda item: group_value(item, "model"))
        for group in groups:
            lines.append(f"- Model: {group_value(group, 'model')}")
            lines.extend(render_summary(group, "  "))
    else:
        groups.sort(key=lambda item: (group_value(item, "user"), group_value(item, "model")))
        current_user = None
        for group in groups:
            user = group_value(group, "user")
            if user != current_user:
                lines.append(f"- User: {user}")
                current_user = user
            lines.append(f"  - Model: {group_value(group, 'model')}")
            lines.extend(render_summary(group, "    "))
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
        lines = render_report(report, args.group_by)
    except (ValueError, RuntimeError) as exc:
        print(f"query_accounting.py: {exc}", file=sys.stderr)
        return 1

    print("\n".join(lines))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
