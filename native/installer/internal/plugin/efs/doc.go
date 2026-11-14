// Package efs is the efs plugin: Amazon EFS volumes for actors (the
// ExternalVolumes preview).
//
// Each actor gets its own EFS access point as its data volume, created when
// the actor is created and mounted on whichever node it resumes on. Its AWS
// half is setup-aws's plugin_efs.go (the file system, mount targets,
// security group, the aws-efs-csi-driver add-on and its Pod Identity role);
// this half adds Substrate's own EFS CSI controller, behind mTLS, the
// CSIDriverConfig, the StorageClass efs-ate, and counter-efs templates in
// the counter demo's atespaces.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go. The s3files and openclaw plugins
// require it and use its helpers (kube.go, actors.go).
package efs
