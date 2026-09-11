#!/usr/bin/env python3
"""Transfer configuration through Botmux's administrator API (Python stdlib)."""
import argparse
import http.client
import json
import os
import re
from pathlib import Path
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("redirect refused")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique_object,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("invalid number")))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("export", "restore"))
    parser.add_argument("--url", required=True)
    parser.add_argument("--file", default="settings/botmux.json")
    args = parser.parse_args()
    url = urllib.parse.urlsplit(args.url)
    if url.scheme not in ("http", "https") or not url.netloc or url.username or url.password or url.query or url.fragment:
        raise ValueError("invalid instance URL")
    headers = {"Accept": "application/json"}
    key, cookie = os.environ.get("BOTMUX_ADMIN_KEY"), os.environ.get("BOTMUX_ADMIN_COOKIE")
    if key:
        headers["Authorization"] = "Bearer " + key
    elif cookie:
        headers["Cookie"] = cookie
    else:
        raise ValueError("set BOTMUX_ADMIN_KEY or BOTMUX_ADMIN_COOKIE")
    path = Path(args.file)
    data = None
    if args.action == "restore":
        data = path.read_bytes()
        snapshot = decode(data)
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(args.url.rstrip("/") + "/api/config/" + args.action,
                                     data=data, headers=headers)
    with urllib.request.build_opener(NoRedirect).open(request, timeout=120) as response:
        if response.status != 200:
            raise ValueError("unexpected HTTP status")
        raw = response.read(16 * 1024 * 1024 + 1)
        if len(raw) > 16 * 1024 * 1024:
            raise ValueError("response too large")
        length = response.headers.get("Content-Length")
        if length is not None and len(raw) != int(length):
            raise ValueError("incomplete download")
    document = decode(raw)
    if args.action == "export":
        if not isinstance(document, dict) or document.get("schema_version") != 1 or not all(
            isinstance(document.get(k), list) for k in ("bots", "destinations", "conditional_routes",
                "notification_services", "subscriptions", "business_routes", "workloads")
        ):
            raise ValueError("invalid snapshot response")
        path.parent.mkdir(parents=True, exist_ok=True)
        temp = None
        try:
            with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as output:
                temp = output.name
                output.write(raw)
                output.flush()
                os.fsync(output.fileno())
            os.replace(temp, path)
            temp = None
        finally:
            if temp is not None:
                os.unlink(temp)
        print(f"Exported {len(document['bots'])} Bots and {len(document['destinations'])} destinations")
    else:
        if not isinstance(document, dict) or document.get("configuration_committed") is not True:
            raise ValueError("invalid restore receipt")
        # Print only typed counts/status; never echo arbitrary response text.
        bots, destinations = document.get("bots"), document.get("destinations")
        if type(bots) is not int or type(destinations) is not int:
            raise ValueError("invalid restore counts")
        loaded = document.get("runtime_loaded") is True
        print(f"Configuration committed: {bots} Bots, {destinations} destinations; "
              f"runtime loaded: {loaded}; external health: not verified")
        if not loaded:
            refs = document.get("runtime_failed_refs")
            expected = {bot.get("ref") for bot in snapshot.get("bots", []) if isinstance(bot, dict)}
            if isinstance(refs, list) and all(
                isinstance(ref, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,128}", ref) and ref in expected
                for ref in refs
            ):
                print("Runtime load failed for Bot refs: " + ", ".join(refs), file=sys.stderr)
            return 1
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except urllib.error.HTTPError as error:
        message = f"Configuration request failed: HTTP {error.code}"
        try:
            detail = decode(error.read(4096))
            codes = {"invalid_snapshot", "unsupported_configuration", "target_conflict", "storage_unavailable"}
            if isinstance(detail, dict) and isinstance(detail.get("code"), str) and detail["code"] in codes:
                message += " (" + detail["code"] + ")"
                location = detail.get("location")
                fields = ("schema_version|bots|destinations|conditional_routes|notification_services|"
                          "subscriptions|business_routes|workloads|ref|bot_ref|name|token|username|"
                          "description|manage_enabled|proxy_enabled|long_poll_enabled|disabled|"
                          "backend_url|secret_token|polling_timeout|source|chat_id|status")
                if isinstance(location, str) and re.fullmatch(
                    r"snapshot(?:\.(?:" + fields + r")(?:\[[0-9]+\])?)*", location
                ):
                    message += " at " + location
        except (ValueError, OSError, http.client.HTTPException):
            pass
        print(message, file=sys.stderr)
    except (OSError, ValueError, urllib.error.URLError, http.client.HTTPException):
        print("Configuration operation failed: check authentication, file, format and connection", file=sys.stderr)
    sys.exit(1)
