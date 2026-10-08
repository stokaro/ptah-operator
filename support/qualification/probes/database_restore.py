#!/usr/bin/env python3
"""Disposable database backup transport preflight, not operator acceptance."""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import time

IMAGES = {
    "postgresql": "postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73",
    "mysql": "mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb",
}
DOCKER_CONTEXT = os.environ.get("DOCKER_CONTEXT") or "remote-dev-container"
DOCKER = ["docker", "--context", DOCKER_CONTEXT]


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def digest(data):
    return hashlib.sha256(data).hexdigest()


class Probe:
    def __init__(self, engine, root):
        self.engine = engine
        self.root = root
        root.mkdir(mode=0o700)
        self.prefix = "ptah-020-restore-" + secrets.token_hex(5)
        self.containers = []
        self.volumes = []
        self.network = None
        self.env_files = []
        self.mysql_client = None
        self.report = {
            "scope": "Isolated idle database backup, encryption and fresh-instance restoration only; no operator, migration history, full recovery RPO/RTO, final-artifact or production-store qualification",
            "engine": engine, "image": IMAGES[engine], "context": DOCKER_CONTEXT,
            "runID": self.prefix, "startedAt": now(), "status": "RUNNING",
            "sourceCommit": subprocess.check_output(["git", "rev-parse", "HEAD"]).decode().strip(),
            "procedureSHA256": digest(Path(__file__).read_bytes()),
            "consistencyAssumptions": "Disposable isolated source; schema and grants do not change during backup; one declared data write occurs after backup completion",
            "steps": [], "checks": {}, "backups": {},
        }

    def persist(self):
        (self.root / "result.json").write_text(json.dumps(self.report, indent=2) + "\n")

    def command(self, action, args, data=None, required=True):
        row = {"action": action, "startedAt": now()}
        result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=180)
        row.update(completedAt=now(), exitCode=result.returncode)
        self.report["steps"].append(row)
        if result.stderr:
            (self.root / (str(len(self.report["steps"])) + ".stderr.private")).write_bytes(result.stderr)
        self.persist()
        if required and result.returncode:
            raise RuntimeError(action + " failed; private diagnostics retained")
        return result

    def check(self, name, condition):
        self.report["checks"][name] = bool(condition)
        self.persist()
        if not condition:
            raise RuntimeError(name)

    def env(self, name, values):
        path = self.root / (name + ".env.private")
        path.write_text("".join(k + "=" + v + "\n" for k, v in values.items()))
        self.env_files.append(path)
        return path

    def sql(self, container, query, env=None, required=True, database="drill"):
        args = DOCKER + ["exec", "-i"]
        client_env = env if env else self.mysql_client
        if client_env:
            args += ["--env-file", str(client_env)]
        args += [container]
        if self.engine == "postgresql":
            args += ["psql", "-X", "-h", "127.0.0.1", "-v", "ON_ERROR_STOP=1", "-At", "-d", database]
        else:
            args += ["mysql", "--protocol=TCP", "--host=127.0.0.1", "--batch", "--skip-column-names", "--user=" + ("drill_reader" if env else "root")]
            if database:
                args += [database]
        return self.command("execute " + ("reader" if env else "administrator") + " SQL", args, query.encode(), required)

    def start(self, name, password, source):
        container, volume = self.prefix + "-" + name, self.prefix + "-" + name + "-data"
        self.command("create named volume", DOCKER + ["volume", "create", "--label", "ptah.restore-run=" + self.prefix, volume])
        self.volumes.append(volume)
        if self.engine == "postgresql":
            user = "source_admin" if source else "restore_admin"
            env = {"POSTGRES_USER": user, "POSTGRES_PASSWORD": password, "POSTGRES_DB": "drill" if source else "postgres", "PGUSER": user, "PGPASSWORD": password}
            mount = "/var/lib/postgresql/data"
        else:
            # MYSQL_PWD on the server breaks the entrypoint's initial passwordless
            # root connection. Supply it only to clients after initialization.
            env = {"MYSQL_ROOT_PASSWORD": password}
            mount = "/var/lib/mysql"
        file = self.env(name, env)
        # Register before starting, so a failed start still gets exact cleanup.
        self.containers.append(container)
        self.command("start " + name, DOCKER + ["run", "-d", "--name", container, "--label", "ptah.restore-run=" + self.prefix, "--network", self.network, "--env-file", str(file), "--mount", "type=volume,source=" + volume + ",target=" + mount, IMAGES[self.engine]])
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            ready = self.sql(container, "SELECT 1;", required=False, database="postgres" if self.engine == "postgresql" else "")
            if ready.returncode == 0 and ready.stdout.strip() == b"1":
                break
            state = self.command("check starting container state", DOCKER + ["inspect", "--format", "{{.State.Status}}", container]).stdout.strip()
            if state in (b"exited", b"dead"):
                raise RuntimeError("database container exited during initialization")
            time.sleep(2)
        else:
            raise RuntimeError("database did not become ready")
        return container, volume

    def encrypted(self, name, data, recipient, key, wrong):
        path = self.root / (name + ".age")
        encrypted = self.command("encrypt " + name, ["age", "-r", recipient], data).stdout
        path.write_bytes(encrypted)
        denied = self.command("refuse wrong backup identity", ["age", "-d", "-i", str(wrong), str(path)], required=False)
        self.check(name + " wrong identity refused", denied.returncode != 0 and not denied.stdout and b"no identity matched" in denied.stderr)
        restored = self.command("decrypt " + name, ["age", "-d", "-i", str(key), str(path)]).stdout
        self.check(name + " decrypted bytes match", restored == data and len(data) > 0)
        self.report["backups"][name] = {"plaintextSHA256": digest(data), "ciphertextSHA256": digest(encrypted), "plaintextBytes": len(data), "ciphertextBytes": len(encrypted)}
        self.persist()
        return path

    def inspect(self, container):
        rows = self.sql(container, "SELECT id, value FROM recovery_canary ORDER BY id;").stdout
        if self.engine == "postgresql":
            schema = self.sql(container, "SELECT column_name,data_type,is_nullable,column_default FROM information_schema.columns WHERE table_schema='public' AND table_name='recovery_canary' ORDER BY ordinal_position; SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='recovery_canary' ORDER BY indexname;").stdout
            access = self.sql(container, "SELECT tablename,tableowner FROM pg_tables WHERE schemaname='public' ORDER BY tablename; SELECT relname,relacl FROM pg_class WHERE oid='public.recovery_canary'::regclass; SELECT rolname,rolsuper,rolcanlogin FROM pg_roles WHERE rolname IN ('source_admin','drill_reader') ORDER BY rolname;").stdout
        else:
            schema = self.sql(container, "SHOW CREATE TABLE recovery_canary;").stdout
            access = self.sql(container, "SHOW GRANTS FOR 'drill_reader'@'%'; SELECT user,host,plugin,account_locked FROM mysql.user WHERE user='drill_reader' ORDER BY host;").stdout
        return {"rows": rows, "schema": schema, "access": access}

    def run(self):
        try:
            self.command("create isolated network", DOCKER + ["network", "create", "--internal", "--label", "ptah.restore-run=" + self.prefix, self.prefix])
            self.network = self.prefix
            image = self.command("record actual image identity", DOCKER + ["image", "inspect", "--format", "{{.Id}} {{.Architecture}}", IMAGES[self.engine]]).stdout.decode().strip()
            self.report["actualImage"] = image
            password, reader_password = secrets.token_hex(24), secrets.token_hex(24)
            if self.engine == "mysql":
                self.mysql_client = self.env("administrator-client", {"MYSQL_PWD": password})
            reader = self.env("reader", {"PGUSER": "drill_reader", "PGPASSWORD": reader_password} if self.engine == "postgresql" else {"MYSQL_PWD": reader_password})
            source, volume = self.start("source", password, True)
            if self.engine == "postgresql":
                fixture = "CREATE ROLE drill_reader LOGIN PASSWORD '" + reader_password + "'; CREATE TABLE recovery_canary(id integer PRIMARY KEY, value text NOT NULL); INSERT INTO recovery_canary VALUES(1,'before-backup'),(2,'preserve-this-row'); GRANT CONNECT ON DATABASE drill TO drill_reader; GRANT USAGE ON SCHEMA public TO drill_reader; GRANT SELECT ON recovery_canary TO drill_reader;"
            else:
                self.sql(source, "CREATE DATABASE drill;", database="")
                fixture = "CREATE USER 'drill_reader'@'%' IDENTIFIED BY '" + reader_password + "'; CREATE TABLE recovery_canary(id integer PRIMARY KEY, value text NOT NULL) ENGINE=InnoDB; INSERT INTO recovery_canary VALUES(1,'before-backup'),(2,'preserve-this-row'); GRANT SELECT ON drill.* TO 'drill_reader'@'%';"
            self.sql(source, fixture)
            before = self.inspect(source)
            self.check("source fixture has both expected rows", before["rows"].decode().splitlines() == (["1|before-backup", "2|preserve-this-row"] if self.engine == "postgresql" else ["1\tbefore-backup", "2\tpreserve-this-row"]))
            self.check("source reader can read", self.sql(source, "SELECT count(*) FROM recovery_canary;", reader).stdout.strip() == b"2")
            key, wrong = self.root / "restore-identity.private", self.root / "wrong-identity.private"
            self.command("create backup identity", ["age-keygen", "-o", str(key)])
            self.command("create wrong backup identity", ["age-keygen", "-o", str(wrong)])
            recipient = self.command("read backup recipient", ["age-keygen", "-y", str(key)]).stdout.decode().strip()
            self.report["backupStartedAt"] = now()
            if self.engine == "postgresql":
                roles = self.command("back up database roles", DOCKER + ["exec", source, "pg_dumpall", "-h", "127.0.0.1", "--roles-only"]).stdout
                archive = self.command("back up database archive", DOCKER + ["exec", source, "pg_dump", "-h", "127.0.0.1", "-Fc", "-d", "drill"]).stdout
                paths = [("roles", self.encrypted("roles", roles, recipient, key, wrong)), ("database", self.encrypted("database", archive, recipient, key, wrong))]
                del roles, archive
            else:
                archive = self.command("back up databases and grant tables", DOCKER + ["exec", "--env-file", str(self.mysql_client), source, "mysqldump", "--protocol=TCP", "--host=127.0.0.1", "--user=root", "--all-databases", "--single-transaction", "--routines", "--events", "--triggers", "--flush-privileges", "--set-gtid-purged=OFF"]).stdout
                paths = [("database", self.encrypted("database", archive, recipient, key, wrong))]
                del archive
            self.report["backupCompletedAt"] = now()
            self.sql(source, "INSERT INTO recovery_canary VALUES(3,'after-backup-expected-loss');")
            self.report["lateCommitAcknowledgedAt"] = now()
            self.check("late commit exists before loss", self.sql(source, "SELECT count(*) FROM recovery_canary;").stdout.strip() == b"3")
            before_loss_ids = set(self.sql(source, "SELECT id FROM recovery_canary ORDER BY id;").stdout.decode().splitlines())
            self.report["lossInjectedAt"] = now()
            loss_clock = time.monotonic()
            self.command("destroy source database container", DOCKER + ["rm", "-f", source])
            self.containers.remove(source)
            self.command("destroy source database volume", DOCKER + ["volume", "rm", volume])
            self.volumes.remove(volume)
            self.check("source container no longer exists", self.command("check destroyed source", DOCKER + ["inspect", source], required=False).returncode != 0)
            self.check("source volume no longer exists", self.command("check destroyed source volume", DOCKER + ["volume", "inspect", volume], required=False).returncode != 0)
            target, _ = self.start("restored", password, False)
            if self.engine == "postgresql":
                absent = self.sql(target, "SELECT count(*) FROM pg_database WHERE datname='drill'; SELECT count(*) FROM pg_roles WHERE rolname='drill_reader';", database="postgres").stdout.strip()
            else:
                absent = self.sql(target, "SELECT count(*) FROM information_schema.schemata WHERE schema_name='drill'; SELECT count(*) FROM mysql.user WHERE user='drill_reader';", database="").stdout.strip()
            self.check("fresh target has no fixture database or reader", absent == b"0\n0")
            for name, path in paths:
                data = self.command("read retained encrypted " + name, ["age", "-d", "-i", str(key), str(path)]).stdout
                if self.engine == "postgresql":
                    if name == "roles":
                        self.sql(target, data.decode(), database="postgres")
                    else:
                        self.command("restore database ownership and grants", DOCKER + ["exec", "-i", target, "pg_restore", "-h", "127.0.0.1", "--exit-on-error", "--create", "-d", "postgres"], data)
                else:
                    self.sql(target, data.decode(), database="")
                del data
            self.report["restoreCompletedAt"] = now()
            after = self.inspect(target)
            after_ids = set(self.sql(target, "SELECT id FROM recovery_canary ORDER BY id;").stdout.decode().splitlines())
            lost_ids = sorted(int(value) for value in before_loss_ids - after_ids)
            self.check("observed loss is exactly the declared post-backup row", lost_ids == [3] and not (after_ids - before_loss_ids))
            self.report["inventories"] = {}
            for name in before:
                self.check("restored " + name + " matches recovery point", before[name] == after[name] and len(before[name]) > 0)
                self.report["inventories"][name] = {"beforeSHA256": digest(before[name]), "afterSHA256": digest(after[name])}
            self.check("restored reader credential and SELECT work", self.sql(target, "SELECT count(*) FROM recovery_canary;", reader).stdout.strip() == b"2")
            refused = self.sql(target, "CREATE TABLE forbidden_by_grants(id integer);", reader, required=False)
            signature = b"permission denied for schema public" if self.engine == "postgresql" else b"ERROR 1142"
            self.check("restored read-only identity refuses DDL for permission reason", refused.returncode != 0 and signature in refused.stderr)
            self.sql(target, "ALTER TABLE recovery_canary ADD COLUMN recovered integer; UPDATE recovery_canary SET recovered=1;")
            self.check("fresh authorized change works after restore", self.sql(target, "SELECT count(*) FROM recovery_canary WHERE recovered=1;").stdout.strip() == b"2")
            self.report.update(expectedLostRowIDs=[3], observedLostRowIDs=lost_ids, databaseValidationCompletedAt=now(), databasePreflightRecoverySeconds=round(time.monotonic()-loss_clock, 3), status="PASS")
            self.persist()
        except Exception as exc:
            self.report.update(status="FAIL", failure=str(exc))
            self.persist()
            raise
        finally:
            clean = True
            for container in reversed(self.containers):
                clean &= self.command("remove owned container", DOCKER + ["rm", "-f", container], required=False).returncode == 0
            for volume in reversed(self.volumes):
                clean &= self.command("remove owned volume", DOCKER + ["volume", "rm", volume], required=False).returncode == 0
            if self.network:
                clean &= self.command("remove owned network", DOCKER + ["network", "rm", self.network], required=False).returncode == 0
            for file in self.env_files:
                file.unlink(missing_ok=True)
            (self.root / "wrong-identity.private").unlink(missing_ok=True)
            self.report.update(cleanupSucceeded=bool(clean), completedAt=now(), retainedPrivateMaterial="Encrypted fixture backups, age restore identity and private diagnostic stderr; disposable database env credentials removed")
            self.persist()
            if not clean:
                raise RuntimeError("owned resource cleanup failed")


if __name__ == "__main__":
    os.umask(0o077)
    parser = argparse.ArgumentParser()
    parser.add_argument("engine", choices=IMAGES)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    probe = Probe(args.engine, args.output)
    probe.run()
    print(json.dumps({"engine": args.engine, "status": probe.report["status"], "checks": len(probe.report["checks"]), "databasePreflightRecoverySeconds": probe.report["databasePreflightRecoverySeconds"], "cleanupSucceeded": probe.report["cleanupSucceeded"], "report": str(args.output / "result.json")}))
