#!/usr/bin/env bash
# Creates Substrate's roles, schema and login users on the private RDS
# instance, by running pkg/postgressetup/setup.sql from a short-lived Job
# inside the cluster (the database has no public endpoint).
#
# Credentials come from Secrets Manager:
#   rds!db-...                 RDS-managed master secret (admin, bootstrap only)
#   <cluster>/ate-owner         owner login (DDL / migrations)
#   <cluster>/ate-readwrite     runtime login (DML)
# The admin password only lives in a Secret in a throwaway namespace for the
# length of the Job; the namespace is deleted afterwards, also on failure.
# Passwords never appear in a command line, here or in the Job.
#
# deploy.sh runs it after substrate/fetch.sh, which provides setup.sql, and
# demos/db-rotation/deploy.sh runs it again before the first rotation. A Job
# runs a pod until it completes, then stops.
#
# Learn more:
#   https://kubernetes.io/docs/concepts/workloads/controllers/job/
#   https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Appendix.PostgreSQL.CommonDBATasks.Roles.html
#   https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html
#   https://github.com/agent-substrate/substrate/blob/v1.0.0-rc.3/docs/guides/postgres.md
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"
NS=ate-db-bootstrap
IMAGE=public.ecr.aws/docker/library/postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5
secret_json() { aws_ secretsmanager get-secret-value --secret-id "$1" --query SecretString --output text; }
field() { python3 -c 'import sys,json;print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }
[ -f "$SUBSTRATE_SRC/pkg/postgressetup/setup.sql" ] || die "no Substrate source at $SUBSTRATE_SRC; run substrate/fetch.sh"

log "Wait for $DB_ID to be available"
aws_ rds wait db-instance-available --db-instance-identifier "$DB_ID"
host=$(aws_ rds describe-db-instances --db-instance-identifier "$DB_ID" --query 'DBInstances[0].Endpoint.Address' --output text)
master_arn=$(aws_ rds describe-db-instances --db-instance-identifier "$DB_ID" --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text)
admin=$(secret_json "$master_arn")
owner=$(secret_json "$SECRET_PREFIX/ate-owner")
rw=$(secret_json "$SECRET_PREFIX/ate-readwrite")
# Secrets Manager rotation (demos/db-rotation) alternates each login between
# USER and USER_clone. Make both with the attributes setup.sql requires
# before it runs: setup.sql manages whichever one the secret names now, and
# the rotation function only sets the other one's password.
base() { field username | sed 's/_clone$//'; }
owner_base=$(base <<<"$owner"); rw_base=$(base <<<"$rw")
cat > "$WORK/db-alternates.sql" <<EOF
DO \$\$
DECLARE r record;
BEGIN
  FOR r IN SELECT * FROM (VALUES ('substrate_owner'), ('substrate_readwrite')) v(name) LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r.name) THEN
      EXECUTE format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION', r.name);
    END IF;
  END LOOP;
  FOR r IN SELECT * FROM (VALUES
      ('$owner_base', 'substrate_owner'), ('${owner_base}_clone', 'substrate_owner'),
      ('$rw_base', 'substrate_readwrite'), ('${rw_base}_clone', 'substrate_readwrite')) v(name, grp) LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r.name) THEN
      EXECUTE format('CREATE ROLE %I LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION', r.name);
    ELSE
      EXECUTE format('ALTER ROLE %I NOINHERIT', r.name);
    END IF;
    EXECUTE format('GRANT %I TO %I', r.grp, r.name);
  END LOOP;
END
\$\$;
EOF

log "Bootstrap roles and schema from an in-cluster Job"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# The namespace holds the master password: remove it however this ends.
trap 'kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT
# The credentials reach kubectl on stdin, never as command-line arguments.
printf '%s\n%s\n%s\n' "$admin" "$owner" "$rw" | python3 -c '
import sys, json, base64
a, o, r = (json.loads(l) for l in sys.stdin.read().splitlines())
d = {"PGUSER": a["username"], "PGPASSWORD": a["password"], "OWNER_USER": o["username"],
     "OWNER_PW": o["password"], "RW_USER": r["username"], "RW_PW": r["password"]}
print(json.dumps({"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
  "metadata": {"name": "creds", "namespace": sys.argv[1]},
  "data": {k: base64.b64encode(v.encode()).decode() for k, v in d.items()}}))' "$NS" \
  | kubectl apply -f - >/dev/null
unset admin owner rw
# The ConfigMap carries the SQL files and the RDS CA bundle to the Job's
# /setup mount.
kubectl -n "$NS" create configmap setup \
  --from-file=setup.sql="$SUBSTRATE_SRC/pkg/postgressetup/setup.sql" \
  --from-file=alternates.sql="$WORK/db-alternates.sql" \
  --from-file=rds-ca.pem="$WORK/rds-global-bundle.pem" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

kubectl -n "$NS" delete job bootstrap --ignore-not-found >/dev/null
# PGSSLMODE=verify-full: psql checks that the server certificate chains to
# the RDS CA bundle and matches the host name.
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata:
  name: bootstrap
  namespace: $NS
spec:
  backoffLimit: 2
  template:
    spec:
      restartPolicy: Never
      nodeSelector: { ate.dev/pool: system }
      securityContext: { runAsNonRoot: true, runAsUser: 70, seccompProfile: { type: RuntimeDefault } }
      containers:
      - name: psql
        image: $IMAGE
        securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
        envFrom: [{ secretRef: { name: creds } }]
        env:
        - { name: PGHOST, value: "$host" }
        - { name: PGDATABASE, value: atepg }
        - { name: PGSSLMODE, value: verify-full }
        - { name: PGSSLROOTCERT, value: /setup/rds-ca.pem }
        command: ["sh", "-c"]
        args:
        # RDS's master user is rds_superuser, not a superuser: it may only
        # create a schema owned by a role it can SET ROLE to, so make the
        # owner role first and grant it to the master user.
        - >-
          psql --no-psqlrc -v ON_ERROR_STOP=1 -c
          "DO \\\$\\\$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='substrate_owner') THEN
          CREATE ROLE substrate_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION; END IF;
          EXECUTE format('GRANT substrate_owner TO %I', current_user); END\\\$\\\$;"
          && psql --no-psqlrc -v ON_ERROR_STOP=1 -f /setup/alternates.sql
          && { printf '%s\\n' '\\getenv substrate_owner_user OWNER_USER' '\\getenv substrate_owner_password OWNER_PW'
          '\\getenv substrate_readwrite_user RW_USER' '\\getenv substrate_readwrite_password RW_PW';
          cat /setup/setup.sql; }
          | psql --no-psqlrc --single-transaction -v ON_ERROR_STOP=1
          -v substrate_schema=substrate
          -v substrate_owner_role=substrate_owner
          -v substrate_readwrite_role=substrate_readwrite
          -f -
          && psql --no-psqlrc -c '\dn substrate' -c '\du substrate*'
        volumeMounts: [{ name: setup, mountPath: /setup }]
      volumes: [{ name: setup, configMap: { name: setup } }]
EOF
kubectl -n "$NS" wait --for=condition=complete job/bootstrap --timeout=180s \
  || { kubectl -n "$NS" logs job/bootstrap; exit 1; }
kubectl -n "$NS" logs job/bootstrap
kubectl delete namespace "$NS" --timeout=2m >/dev/null
trap - EXIT
info "bootstrap done; $NS deleted"
