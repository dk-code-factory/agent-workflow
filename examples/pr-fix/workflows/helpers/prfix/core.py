"""Bounded JSON, immutable artifacts and transactional application bookkeeping.

This database is application state. It contains no controller records, provider
sessions, credentials or engine ownership capabilities.
"""

from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import tempfile
import time
from urllib.parse import urlsplit

MAX_JSON = 1 << 20
STATE = ".pr-fix-state"
GENERATED = "workflows/.pr-fix-portable"
FORMAT = "portable-pr-fix-v1"


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False, allow_nan=False).encode("utf-8")


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def text(value, label="text"):
    if not isinstance(value, str) or not value.strip() or "\0" in value:
        raise ValueError(label + " must be a nonempty string without NUL")
    value.encode("utf-8", "strict")
    return value


def auth(value):
    if not isinstance(value, dict) or set(value) != {"request_id", "instruction", "source"}:
        raise ValueError("authorization needs request_id, exact instruction and source")
    return {key: text(value[key], "authorization " + key) for key in value}


def identity(raw):
    parsed = urlsplit(text(raw, "PR URL"))
    match = re.fullmatch(r"/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([1-9][0-9]*)", parsed.path)
    if (parsed.scheme != "https" or not re.fullmatch(r"[A-Za-z0-9.-]+", parsed.netloc)
            or parsed.query or parsed.fragment or not match
            or match[1] in {".", ".."} or match[2] in {".", ".."}):
        raise ValueError("canonical HTTPS pull request URL required")
    host, owner, name = parsed.netloc.lower(), match[1].lower(), match[2].lower()
    number = int(match[3])
    repository = "/".join((host, owner, name))
    return {"url": f"https://{repository}/pull/{number}", "host": host,
            "owner": owner, "name": name, "repository": repository, "number": number}


def bounded_json(raw, limit=MAX_JSON):
    if len(raw.encode("utf-8") if isinstance(raw, str) else raw) > limit:
        raise ValueError("JSON exceeds byte limit")
    def pairs(entries):
        result = {}
        for key, value in entries:
            if key in result:
                raise ValueError("duplicate JSON key: " + key)
            result[key] = value
        return result
    def constant(value):
        raise ValueError("nonfinite JSON number: " + value)
    return json.loads(raw, object_pairs_hook=pairs, parse_constant=constant)


def atomic_json(path, value):
    path = Path(path)
    data = canonical(value)
    if len(data) > 16 << 20:
        raise ValueError("artifact exceeds byte limit")
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if path.is_symlink():
        raise ValueError("artifact must not be a symlink")
    descriptor, name = tempfile.mkstemp(prefix=".staged-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        Path(name).unlink(missing_ok=True)


def owned_directory(root, name, create=False):
    root = Path(root).resolve(strict=True)
    path = root / name
    for parent in [path, *path.parents]:
        if parent == root:
            break
        if parent.is_symlink():
            raise ValueError("application directory must not traverse a symlink")
    marker = path / ".owned-by-portable-pr-fix"
    if not path.exists():
        if not create:
            raise ValueError("application state has not been prepared")
        path.mkdir(mode=0o700, parents=True)
        atomic_json(marker, {"format": FORMAT})
    if not path.is_dir() or marker.is_symlink() or not marker.is_file():
        raise ValueError("refusing unrelated application directory")
    if bounded_json(marker.read_bytes()) != {"format": FORMAT}:
        raise ValueError("application ownership marker differs")
    return path


def artifact(root, value):
    path = owned_directory(root, STATE) / "artifacts" / (digest(value) + ".json")
    if path.parent.is_symlink():
        raise ValueError("artifact directory must not be a symlink")
    if path.exists():
        if path.is_symlink() or path.read_bytes() != canonical(value):
            raise ValueError("immutable artifact conflict")
    else:
        atomic_json(path, value)
    return path.relative_to(Path(root).resolve()).as_posix()


def read_artifact(root, reference):
    root = Path(root).resolve(strict=True)
    relative = Path(text(reference, "artifact reference"))
    if relative.is_absolute() or ".." in relative.parts:
        raise ValueError("invalid artifact reference")
    base = owned_directory(root, STATE) / "artifacts"
    if base.is_symlink():
        raise ValueError("artifact directory must not be a symlink")
    path = root / relative
    if path.parent != base or path.is_symlink() or not re.fullmatch(r"[a-f0-9]{64}\.json", path.name):
        raise ValueError("artifact is outside application-owned artifacts")
    value = bounded_json(path.read_bytes(), 16 << 20)
    if path.name != digest(value) + ".json":
        raise ValueError("artifact identity differs")
    return value


SCHEMA = """
CREATE TABLE IF NOT EXISTS groups(id TEXT PRIMARY KEY, pr TEXT NOT NULL, created REAL NOT NULL);
CREATE TABLE IF NOT EXISTS reservations(name TEXT NOT NULL, operation TEXT NOT NULL,
 amount INTEGER NOT NULL, limit_value INTEGER NOT NULL, PRIMARY KEY(name,operation));
CREATE TABLE IF NOT EXISTS deadlines(name TEXT PRIMARY KEY, value REAL NOT NULL);
CREATE TABLE IF NOT EXISTS extensions(id TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS claims(id TEXT PRIMARY KEY, task TEXT NOT NULL, fingerprint TEXT NOT NULL,
 state TEXT NOT NULL, agent TEXT NOT NULL DEFAULT '', assignment TEXT NOT NULL,
 report TEXT NOT NULL DEFAULT '', coverage TEXT NOT NULL DEFAULT '', UNIQUE(task,fingerprint));
CREATE TABLE IF NOT EXISTS submissions(claim_id TEXT NOT NULL, round INTEGER NOT NULL,
 agent TEXT NOT NULL, report TEXT NOT NULL, PRIMARY KEY(claim_id,round));
CREATE TABLE IF NOT EXISTS accepted_reports(reference TEXT PRIMARY KEY, task TEXT NOT NULL,
 kind TEXT NOT NULL, fingerprint TEXT NOT NULL, agent TEXT NOT NULL, report TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS operations(id TEXT PRIMARY KEY, kind TEXT NOT NULL, input TEXT NOT NULL,
 state TEXT NOT NULL, result TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS publication_intents(id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL,
 value TEXT NOT NULL, state TEXT NOT NULL, receipt TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS publication_aliases(id TEXT PRIMARY KEY, canonical_id TEXT NOT NULL,
 fingerprint TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS local_commits(id TEXT PRIMARY KEY,parent TEXT NOT NULL,
 head TEXT NOT NULL,tree TEXT NOT NULL,message TEXT NOT NULL,state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS native_failures(claim_id TEXT NOT NULL, round INTEGER NOT NULL,
 agent TEXT NOT NULL, classification TEXT NOT NULL, unsafe TEXT NOT NULL,
 diagnostic TEXT NOT NULL, PRIMARY KEY(claim_id,round));
CREATE TABLE IF NOT EXISTS ci_observations(operation TEXT PRIMARY KEY,head TEXT NOT NULL,
 status TEXT NOT NULL,result TEXT NOT NULL);
"""


class Ledger:
    def __init__(self, root, logical_run, pr=None, read_only=False):
        if not re.fullmatch(r"[A-Za-z0-9_-]{1,100}", text(logical_run, "logical run")):
            raise ValueError("invalid logical run")
        directory = owned_directory(root, STATE, create=not read_only)
        path = directory / (logical_run + ".sqlite")
        if path.is_symlink():
            raise ValueError("ledger must not be a symlink")
        self.read_only = read_only
        self.root = root
        self.logical_run = logical_run
        self.db = sqlite3.connect(path.as_uri() + ("?mode=ro" if read_only else "?mode=rwc"),
                                  uri=True, isolation_level=None, timeout=15)
        try:
            self.db.row_factory = sqlite3.Row
            self.db.execute("PRAGMA foreign_keys=ON")
            self.db.execute("PRAGMA busy_timeout=15000")
            if not read_only:
                os.chmod(path, 0o600)
                self.db.execute("PRAGMA journal_mode=WAL")
                self.db.execute("PRAGMA synchronous=FULL")
                self.db.executescript(SCHEMA)
                if pr is not None:
                    with self.transaction():
                        row = self.db.execute("SELECT pr FROM groups WHERE id=?", (logical_run,)).fetchone()
                        if row and row["pr"] != pr:
                            raise ValueError("logical run belongs to another PR")
                        self.db.execute("INSERT OR IGNORE INTO groups VALUES(?,?,?)",
                                        (logical_run, pr, time.time()))
            elif pr is not None:
                row = self.db.execute("SELECT pr FROM groups WHERE id=?", (logical_run,)).fetchone()
                if not row or row["pr"] != pr:
                    raise ValueError("logical run binding differs")
        except BaseException:
            self.db.close()
            raise

    def close(self):
        self.db.close()

    def __enter__(self):
        return self

    def __exit__(self, *unused):
        self.close()

    @contextmanager
    def transaction(self):
        if self.read_only:
            raise ValueError("read-only hook cannot write application state")
        self.db.execute("BEGIN IMMEDIATE")
        try:
            yield self.db
        except BaseException:
            self.db.rollback()
            raise
        else:
            self.db.commit()

    def reserve(self, name, operation, limit, amount=1):
        text(name, "allowance name")
        text(operation, "allowance operation")
        if type(limit) is not int or type(amount) is not int or limit < 0 or amount < 1:
            raise ValueError("invalid allowance")
        with self.transaction():
            ceiling = self.db.execute("SELECT limit_value FROM reservations WHERE name=? LIMIT 1", (name,)).fetchone()
            if ceiling and ceiling[0] != limit:
                raise ValueError("retained allowance ceiling changed; use explicit continuation")
            old = self.db.execute("SELECT amount,limit_value FROM reservations WHERE name=? AND operation=?",
                                  (name, operation)).fetchone()
            if old:
                if old["amount"] != amount or old["limit_value"] != limit:
                    raise ValueError("allowance operation identity conflict")
                return self.db.execute("SELECT COALESCE(SUM(amount),0) FROM reservations WHERE name=?", (name,)).fetchone()[0]
            used = self.db.execute("SELECT COALESCE(SUM(amount),0) FROM reservations WHERE name=?", (name,)).fetchone()[0]
            extensions = self.db.execute("SELECT value FROM extensions").fetchall()
            extra = sum(bounded_json(row[0]).get("amount", 0) for row in extensions
                        if bounded_json(row[0]).get("name") == name)
            if used + amount > limit + extra:
                raise ValueError("retained allowance exhausted: " + name)
            self.db.execute("INSERT INTO reservations VALUES(?,?,?,?)", (name, operation, amount, limit))
            return used + amount

    def deadline(self, name, seconds):
        if type(seconds) not in {int, float} or seconds <= 0 or seconds > 86400:
            raise ValueError("deadline duration out of bounds")
        with self.transaction():
            row = self.db.execute("SELECT value FROM deadlines WHERE name=?", (name,)).fetchone()
            if row:
                return row[0]
            value = time.time() + seconds
            self.db.execute("INSERT INTO deadlines VALUES(?,?)", (name, value))
            return value

    def extend(self, request_id, name, amount, authorization):
        value = {"name": text(name), "amount": amount, "authorization": auth(authorization)}
        if type(amount) is not int or amount < 1 or amount > 10:
            raise ValueError("continuation amount out of bounds")
        if request_id != value["authorization"]["request_id"]:
            raise ValueError("continuation identity differs from authorization")
        encoded = canonical(value).decode()
        with self.transaction():
            old = self.db.execute("SELECT value FROM extensions WHERE id=?", (request_id,)).fetchone()
            if old and old[0] != encoded:
                raise ValueError("continuation request identity conflict")
            self.db.execute("INSERT OR IGNORE INTO extensions VALUES(?,?)", (request_id, encoded))
        return value


def writer_lease(root, logical_run, claim, *, acquire=False, release=False):
    """Fence the one shared Git index through a whole native writing task.

    This is application bookkeeping, never a controller/session ownership
    replacement. An abandoned writer remains fenced until its original claim
    has a retained completed or verified no-effect terminal disposition.
    """
    directory = owned_directory(root, STATE)
    path = directory / "workspace-writer.sqlite"
    if path.is_symlink():
        raise ValueError("workspace writer journal must not be a symlink")
    if not acquire and not release and not path.exists():
        return
    database = sqlite3.connect(path, isolation_level=None, timeout=15)
    try:
        os.chmod(path, 0o600)
        database.execute("PRAGMA synchronous=FULL")
        database.execute("CREATE TABLE IF NOT EXISTS writer(singleton INTEGER PRIMARY KEY CHECK(singleton=1),logical_run TEXT NOT NULL,claim_id TEXT NOT NULL)")
        database.execute("BEGIN IMMEDIATE")
        old = database.execute("SELECT logical_run,claim_id FROM writer WHERE singleton=1").fetchone()
        if old and old != (logical_run, claim):
            # A crash after task completion but before releasing this local
            # fence may be settled only from that exact original claim.
            with Ledger(root, old[0], read_only=True) as original:
                row = original.db.execute("SELECT state FROM claims WHERE id=?", (old[1],)).fetchone()
                terminal = row and row[0] in {"complete", "optional_failed"}
            if not terminal:
                raise ValueError("unfinished original writer owns the workspace; resume its originating execution")
            database.execute("DELETE FROM writer WHERE singleton=1")
            old = None
        if release:
            if old == (logical_run, claim):
                database.execute("DELETE FROM writer WHERE singleton=1")
        elif acquire:
            database.execute("INSERT OR IGNORE INTO writer VALUES(1,?,?)", (logical_run, claim))
        elif old:
            raise ValueError("unfinished original writer owns the workspace; resume its originating execution")
        database.commit()
    except BaseException:
        database.rollback()
        raise
    finally:
        database.close()


def verify_writer_lease(root, logical_run, claim):
    path = owned_directory(root, STATE) / "workspace-writer.sqlite"
    if path.is_symlink() or not path.is_file():
        raise ValueError("original writer ownership is unavailable")
    database = sqlite3.connect(path.as_uri() + "?mode=ro", uri=True, timeout=15)
    try:
        owner = database.execute("SELECT logical_run,claim_id FROM writer WHERE singleton=1").fetchone()
        if owner != (logical_run, claim):
            raise ValueError("original writer ownership changed")
    finally:
        database.close()
