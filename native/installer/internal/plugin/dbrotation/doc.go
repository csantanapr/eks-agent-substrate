// Package dbrotation is the db-rotation plugin: Secrets Manager rotation of the RDS logins, synced by External Secrets.
//
// Secrets Manager rotates <cluster>/ate-owner and <cluster>/ate-readwrite with
// the AWS-hosted "alternating users" PostgreSQL function; External Secrets
// Operator writes both connection strings into ate-api-server-secret-envvars
// every minute; the ESO Reloader rolls ate-api-server when that Secret
// changes, since it reads the connection strings only at start. Alternating
// users keeps the old login valid while the rollout runs.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go. Its AWS half is setup-aws's
// plugin_db_rotation.go.
package dbrotation
