package npc

const (
	TableFilter = "filter"

	MainChain    = "WEAVE-NPC"
	DefaultChain = "WEAVE-NPC-DEFAULT"
	IngressChain = "WEAVE-NPC-INGRESS"

	EgressChain        = "WEAVE-NPC-EGRESS"
	EgressDefaultChain = "WEAVE-NPC-EGRESS-DEFAULT"
	EgressCustomChain  = "WEAVE-NPC-EGRESS-CUSTOM"

	IpsetNamePrefix = "weave-"

	LocalIpset = IpsetNamePrefix + "local-pods"
)
