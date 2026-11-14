// Package openclaw is the openclaw plugin: the OpenClaw always-on agent on
// Substrate, its gateway state on EFS, the native form of eks-talk's
// demos/openclaw and the always-on-agent repository's demo/eks/deploy-demo.sh.
//
// An always-on gateway holds the chat channel; each conversation's agent is
// a Substrate actor that suspends when idle and resumes on the next message.
// The gateway keeps its state on EFS (StorageClass efs-openclaw, built from
// the efs plugin's file system), which every Availability Zone mounts, so it
// can move to any system node. Nothing is exposed: its Services are
// ClusterIP, reached with kubectl port-forward.
//
// Everything is Go: the images are prebuilt and pinned by digest, the
// repository's manifests are embedded (manifests/, at the commit openclaw.go
// names) and rendered here, and the rest of deploy-demo.sh (the worker
// image pin, atespace and access policy, actor template, golden wait, demo
// actor and its egress policy) is ported, with native Verify and Remove. It
// has no AWS half of its own; the gateway's access point is owner-tagged
// after the add-on's controller makes it.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go.
package openclaw
