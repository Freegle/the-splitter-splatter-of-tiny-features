package router

// Category builds a router_state category key from a turn_type and
// subsystem, matching DESIGN.md's schema comment: "<turn_type>|<subsystem>".
func Category(turnType, subsystem string) string {
	return turnType + "|" + subsystem
}

// FamilyPair builds a router_state families key from a frontier model and a
// local (backend) model, matching DESIGN.md's schema comment:
// "<frontierFamily>><localFamily>".
func FamilyPair(frontierModel, localModel string, overrides map[string]string) string {
	return Family(frontierModel, overrides) + ">" + Family(localModel, overrides)
}
