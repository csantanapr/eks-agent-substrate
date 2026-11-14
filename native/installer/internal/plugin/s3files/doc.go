// Package s3files is the s3files plugin: Amazon S3 Files volumes for actors.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go. Its AWS half is setup-aws's
// plugin_s3files.go.
package s3files
