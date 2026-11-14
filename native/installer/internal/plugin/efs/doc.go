// Package efs is the efs plugin: Amazon EFS volumes for actors (ExternalVolumes).
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go. Its AWS half is setup-aws's
// plugin_efs.go.
package efs
