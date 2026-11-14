package main

// Every plugin registers itself with plugin.Register when its package is
// imported. One line each, so a plugin is added without touching another.
import (
	_ "github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/credprovideraws"
	_ "github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/dbrotation"
	_ "github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
	_ "github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/llmgateway"
	_ "github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
	_ "github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3iso"
	_ "github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/openclaw"
	_ "github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/s3files"
)
