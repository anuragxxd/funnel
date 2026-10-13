package config

import "encoding/gob"

// Both the Funnel server and plugin processes import config, so registering
// these oneof wrappers here keeps net/rpc Config encoding symmetric. Plugin
// binaries must be rebuilt when this config schema changes.
func init() {
	gob.Register(&KubernetesStorageBinding_ExistingClaim{})
	gob.Register(&KubernetesStorageBinding_Templates{})
}
