"""Long-term copies of the usage database: gzip snapshots, JSON/CSV export, import/merge, pruning.

Mirrors go/internal/archive so both editions read and write the same snapshot files
(``watch-my-harness-YYYY-MM-DD-HHMMSS.db.gz``) and the same settings keys.
"""
from __future__ import annotations

import csv
import gzip
import json
import os
import shutil
import sqlite3
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Iterator, Union

DbPath = Union[str, Path]

EXPORT_COLS = ["timestamp", "source", "session_id", "project_slug", "cwd", "model", "type",
               "input_tokens", "output_tokens", "cache_read_tokens",
               "cache_create_5m_tokens", "cache_create_1h_tokens"]
EXPORT_HEAD = ["timestamp", "source", "session_id", "project", "cwd", "model", "type",
               "input_tokens", "output_tokens", "cache_read_tokens",
               "cache_create_5m_tokens", "cache_create_1h_tokens"]


class ArchiveError(Exception):
    pass


def default_dir() -> str:
    return str(Path.home() / ".watch-my-harness" / "archive")


def _conn(db: DbPath) -> sqlite3.Connection:
    c = sqlite3.connect(str(db), timeout=30)
    c.row_factory = sqlite3.Row
    return c


def get_kv(db: DbPath, key: str):
    with _conn(db) as c:
        r = c.execute("SELECT v FROM plan WHERE k=?", (key,)).fetchone()
    return r["v"] if r else None


def set_kv(db: DbPath, key: str, value: str) -> None:
    with _conn(db) as c:
        c.execute("INSERT OR REPLACE INTO plan (k, v) VALUES (?, ?)", (key, value))
        c.commit()


def load(db: DbPath) -> dict:
    s = {"dir": default_dir(), "auto": True, "keep_months": 0, "last_auto": ""}
    if v := get_kv(db, "archive_dir"):
        s["dir"] = v
    if (v := get_kv(db, "archive_auto")) is not None:
        s["auto"] = v == "1"
    if (v := get_kv(db, "archive_keep_months")) is not None:
        try:
            s["keep_months"] = int(v)
        except ValueError:
            pass
    s["last_auto"] = get_kv(db, "archive_last_auto") or ""
    return s


def save(db: DbPath, s: dict) -> None:
    if s["keep_months"] < 0:
        raise ArchiveError("keep_months must be 0 or more")
    d = s["dir"] or default_dir()
    if not os.path.isabs(d):
        raise ArchiveError("archive folder must be an absolute path")
    try:
        os.makedirs(d, exist_ok=True)
    except OSError as e:
        raise ArchiveError(f"cannot use that folder: {e}")
    set_kv(db, "archive_dir", d)
    set_kv(db, "archive_auto", "1" if s["auto"] else "0")
    set_kv(db, "archive_keep_months", str(int(s["keep_months"])))


def list_snapshots(d: str) -> list:
    out = []
    try:
        names = os.listdir(d)
    except OSError:
        return out
    for n in names:
        p = os.path.join(d, n)
        if not n.endswith(".db.gz") or os.path.isdir(p):
            continue
        try:
            st = os.stat(p)
        except OSError:
            continue
        mt = datetime.fromtimestamp(st.st_mtime).astimezone().isoformat(timespec="seconds")
        out.append({"name": n, "bytes": st.st_size, "mtime": mt})
    out.sort(key=lambda x: x["name"], reverse=True)
    return out


def take(db: DbPath, d: str) -> str:
    """Write a consistent gzipped copy of the database into ``d``; returns the file name."""
    os.makedirs(d, exist_ok=True)
    tmp = os.path.join(d, ".snapshot-tmp.db")
    if os.path.exists(tmp):
        os.remove(tmp)
    try:
        src = sqlite3.connect(str(db), timeout=30)
        dst = sqlite3.connect(tmp)
        try:
            src.backup(dst)
        finally:
            dst.close()
            src.close()
        name = "watch-my-harness-" + time.strftime("%Y-%m-%d-%H%M%S") + ".db.gz"
        final = os.path.join(d, name)
        try:
            with open(tmp, "rb") as fin, gzip.open(final + ".part", "wb") as fout:
                shutil.copyfileobj(fin, fout)
            os.replace(final + ".part", final)
        except BaseException:
            if os.path.exists(final + ".part"):
                os.remove(final + ".part")
            raise
        return name
    finally:
        if os.path.exists(tmp):
            os.remove(tmp)


def auto_if_due(db: DbPath) -> None:
    """One snapshot per calendar month when automatic archiving is on. Never raises."""
    try:
        s = load(db)
        month = time.strftime("%Y-%m")
        if not s["auto"] or s["last_auto"] == month:
            return
        with _conn(db) as c:
            if c.execute("SELECT COUNT(*) AS n FROM messages").fetchone()["n"] == 0:
                return
        take(db, s["dir"])
        set_kv(db, "archive_last_auto", month)
    except Exception:
        pass


def _shared_cols(c: sqlite3.Connection, table: str, skip=()) -> str:
    snap = {r["name"] for r in c.execute(f"PRAGMA snap.table_info({table})")}
    main = [r["name"] for r in c.execute(f"PRAGMA main.table_info({table})")]
    cols = [x for x in main if x in snap and x not in skip]
    if not cols:
        raise ArchiveError(f"snapshot has no {table} table")
    return ",".join(cols)


def import_snapshot(db: DbPath, d: str, name: str) -> int:
    """Merge a snapshot made by take() into the live database; existing rows win."""
    name = os.path.basename(name or "")
    if not name.endswith(".db.gz"):
        raise ArchiveError("not a snapshot file")
    src = os.path.join(d, name)
    if not os.path.isfile(src):
        raise ArchiveError(f"no such snapshot: {name}")
    tmp = os.path.join(d, ".import-tmp.db")
    try:
        with gzip.open(src, "rb") as fin, open(tmp, "wb") as fout:
            shutil.copyfileobj(fin, fout)
        c = _conn(db)
        try:
            c.isolation_level = None  # explicit transaction below
            c.execute("ATTACH DATABASE ? AS snap", (tmp,))
            try:
                c.execute("BEGIN")
                mcols = _shared_cols(c, "messages")
                tcols = _shared_cols(c, "tool_calls", skip=("id",))
                # tool_calls first, while "new message" is still decidable.
                c.execute(f"INSERT INTO main.tool_calls ({tcols}) SELECT {tcols} FROM snap.tool_calls "
                          "WHERE message_uuid NOT IN (SELECT uuid FROM main.messages)")
                n = c.execute(f"INSERT OR IGNORE INTO main.messages ({mcols}) "
                              f"SELECT {mcols} FROM snap.messages").rowcount
                c.execute("COMMIT")
            except BaseException:
                if c.in_transaction:
                    c.execute("ROLLBACK")
                raise
            finally:
                c.execute("DETACH DATABASE snap")
        finally:
            c.close()
        return n
    except (OSError, sqlite3.Error, EOFError) as e:
        raise ArchiveError(str(e))
    finally:
        if os.path.exists(tmp):
            os.remove(tmp)


def prune(db: DbPath, keep_months: int) -> int:
    """Delete rows older than ``keep_months``. Callers snapshot first."""
    if keep_months <= 0:
        raise ArchiveError("keep_months must be at least 1 to prune")
    cut = (datetime.now(timezone.utc) - timedelta(days=round(keep_months * 30.4375))
           ).strftime("%Y-%m-%dT%H:%M:%S")
    with _conn(db) as c:
        c.execute("DELETE FROM tool_calls WHERE timestamp < ?", (cut,))
        n = c.execute("DELETE FROM messages WHERE timestamp < ?", (cut,)).rowcount
        c.commit()
    return n


def export_chunks(db: DbPath, fmt: str) -> Iterator[bytes]:
    """Stream messages as csv, or as one JSON array."""
    c = _conn(db)
    try:
        cur = c.execute(f"SELECT {', '.join(EXPORT_COLS)} FROM messages ORDER BY timestamp")
        if fmt == "json":
            yield b"["
            first = True
            for r in cur:
                row = json.dumps(dict(zip(EXPORT_HEAD, tuple(r))), default=str).encode()
                yield (b"" if first else b",\n") + row
                first = False
            yield b"]\n"
            return
        import io
        buf = io.StringIO()
        w = csv.writer(buf, lineterminator="\n")
        w.writerow(EXPORT_HEAD)
        for r in cur:
            w.writerow(["" if v is None else v for v in tuple(r)])
            if buf.tell() > 65536:
                yield buf.getvalue().encode()
                buf.seek(0)
                buf.truncate()
        yield buf.getvalue().encode()
    finally:
        c.close()
