package models

// Permission codenames enforced by the federation gateway.
const (
	PermView    = "federation.view"    // read nodes, resources, log and conflicts
	PermSync    = "federation.sync"    // push changes and pull deltas (edge nodes hold this)
	PermResolve = "federation.resolve" // settle parked conflicts
	PermManage  = "federation.manage"  // register/suspend nodes, change strategy
)

// PermissionDescriptor is one entry registered with iag-authentication at boot.
type PermissionDescriptor struct {
	Name        string
	Description string
}

// PermissionDescriptors is the catalogue this service registers.
//
// Sync is deliberately separate from Manage: an edge node's service account
// needs to push and pull continuously, but must never be able to suspend a
// sibling node or rewrite the conflict policy.
func PermissionDescriptors() []PermissionDescriptor {
	return []PermissionDescriptor{
		{Name: PermView, Description: "View federated nodes, resources, change log and conflicts"},
		{Name: PermSync, Description: "Push changes to and pull deltas from the federation gateway"},
		{Name: PermResolve, Description: "Resolve parked federation conflicts"},
		{Name: PermManage, Description: "Register, suspend and configure federated nodes"},
	}
}
