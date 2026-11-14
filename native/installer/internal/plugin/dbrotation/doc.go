// Package dbrotation is the db-rotation plugin: Secrets Manager rotation of the RDS logins, synced by External Secrets.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go. Its AWS half is setup-aws's
// plugin_db_rotation.go.
package dbrotation
