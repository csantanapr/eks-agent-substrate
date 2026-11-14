// Package s3files is the s3files plugin: Amazon S3 Files volumes for actors.
//
// An S3 file system presents an S3 bucket over NFS. Each actor gets its own
// S3 Files access point through the EFS CSI driver and Substrate controller
// the efs plugin installs (provisioningMode s3files-ap), and its files land
// in the bucket about a minute after they are written. Its AWS half is
// setup-aws's plugin_s3files.go; this half adds the StorageClass
// s3files-ate and counter-s3files templates in the counter demo's
// atespaces.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go.
package s3files
