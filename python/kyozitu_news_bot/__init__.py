from __future__ import annotations

import hashlib
import os
import platform
import shutil
import stat
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

__all__ = ["run"]
__version__ = "0.1.2"

_REPO = "dtmpm3485/kyozitu-News-bot"


def _target() -> tuple[str, str, str]:
    system = platform.system().lower()
    machine = platform.machine().lower()

    # Termux reports Android/aarch64. Android requires a PIE executable,
    # so releases provide a dedicated GOOS=android binary instead of
    # reusing the normal Linux binary.
    os_map = {
        "linux": "linux",
        "android": "android",
        "windows": "windows",
        "darwin": "darwin",
    }
    arch_map = {
        "x86_64": "amd64",
        "amd64": "amd64",
        "aarch64": "arm64",
        "arm64": "arm64",
        "arm64-v8a": "arm64",
    }

    goos = os_map.get(system)
    goarch = arch_map.get(machine)
    if goos is None or goarch is None:
        raise RuntimeError(f"Unsupported platform: {system}/{machine}")

    suffix = ".exe" if goos == "windows" else ""
    return goos, goarch, suffix


def _cache_dir() -> Path:
    override = os.environ.get("KYOZITU_BIN_DIR")
    if override:
        return Path(override).expanduser()
    return Path.home() / ".cache" / "kyozitu-news-bot" / __version__


def _download(url: str, destination: Path) -> None:
    req = urllib.request.Request(url, headers={"User-Agent": f"kyozitu-news-bot/{__version__}"})
    with urllib.request.urlopen(req, timeout=60) as response, destination.open("wb") as f:
        shutil.copyfileobj(response, f)


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def _expected_checksum(text: str, asset_name: str) -> str | None:
    for line in text.splitlines():
        parts = line.strip().split()
        if len(parts) >= 2 and parts[-1].lstrip("*") == asset_name:
            return parts[0].lower()
    return None


def _resolve_binary() -> Path:
    goos, goarch, suffix = _target()
    asset = f"kyozitu-news-bot-{goos}-{goarch}{suffix}"
    cache = _cache_dir()
    binary = cache / asset
    if binary.exists():
        return binary

    cache.mkdir(parents=True, exist_ok=True)
    tag = f"v{__version__}"
    base = f"https://github.com/{_REPO}/releases/download/{tag}"
    tmp = binary.with_suffix(binary.suffix + ".download")

    try:
        _download(f"{base}/{asset}", tmp)
        req = urllib.request.Request(
            f"{base}/checksums.txt",
            headers={"User-Agent": f"kyozitu-news-bot/{__version__}"},
        )
        with urllib.request.urlopen(req, timeout=30) as response:
            checksums = response.read().decode("utf-8", errors="replace")
        expected = _expected_checksum(checksums, asset)
        if not expected:
            raise RuntimeError(f"Checksum for {asset} was not found")
        actual = _sha256(tmp)
        if actual != expected:
            raise RuntimeError(f"Checksum mismatch for {asset}")
        tmp.replace(binary)
    except (urllib.error.URLError, OSError, RuntimeError) as exc:
        try:
            tmp.unlink(missing_ok=True)
        except OSError:
            pass
        raise RuntimeError(
            f"Could not download the Go binary for {goos}/{goarch}: {exc}\n"
            f"Release expected: {base}/{asset}"
        ) from exc

    if goos != "windows":
        binary.chmod(binary.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    return binary


def run(token: str | None = None) -> int:
    """Download (if needed) and run the Go-based 虚実ニュースbot."""
    if token is not None:
        os.environ["DISCORD_BOT_TOKEN"] = token
    if not os.environ.get("DISCORD_BOT_TOKEN"):
        raise RuntimeError(
            "DISCORD_BOT_TOKEN is not set. Example:\n"
            "  from kyozitu_news_bot import run\n"
            "  run(\"YOUR_BOT_TOKEN\")"
        )

    binary = _resolve_binary()
    try:
        return subprocess.call([str(binary)], env=os.environ.copy())
    except KeyboardInterrupt:
        return 130


def main() -> None:
    try:
        raise SystemExit(run())
    except RuntimeError as exc:
        print(f"kyozitu-news-bot: {exc}", file=sys.stderr)
        raise SystemExit(1)
