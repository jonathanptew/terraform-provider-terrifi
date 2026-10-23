package generate

// PortProfile is the subset of a rest/portconf object that maps onto
// terrifi_port_profile, with tagged networks already resolved from the
// controller's exclusion list.
type PortProfile struct {
	ID                       string
	Name                     string
	NativeNetworkID          string
	TaggedVLANMgmt           string
	TaggedNetworkIDs         []string
	PoeMode                  string
	Isolation                bool
	PortSecurityMACAddresses []string
}

// PortProfileBlocks generates import + resource blocks for switch port
// profiles. Attributes equal to the resource defaults are omitted.
func PortProfileBlocks(profiles []PortProfile) []ResourceBlock {
	blocks := make([]ResourceBlock, 0, len(profiles))
	for _, p := range profiles {
		block := ResourceBlock{
			ResourceType: "terrifi_port_profile",
			ResourceName: ToTerraformName(p.Name),
			ImportID:     p.ID,
		}

		block.Attributes = append(block.Attributes, Attr{Key: "name", Value: HCLString(p.Name)})
		if p.NativeNetworkID != "" {
			block.Attributes = append(block.Attributes, Attr{Key: "native_network_id", Value: HCLString(p.NativeNetworkID)})
		}
		mode := p.TaggedVLANMgmt
		if mode == "" {
			mode = "auto"
		}
		if mode != "block_all" {
			block.Attributes = append(block.Attributes, Attr{Key: "tagged_vlan_mgmt", Value: HCLString(mode)})
		}
		if mode == "custom" {
			block.Attributes = append(block.Attributes, Attr{Key: "tagged_network_ids", Value: HCLStringList(p.TaggedNetworkIDs)})
		}
		if p.PoeMode != "" && p.PoeMode != "auto" {
			block.Attributes = append(block.Attributes, Attr{Key: "poe_mode", Value: HCLString(p.PoeMode)})
		}
		if p.Isolation {
			block.Attributes = append(block.Attributes, Attr{Key: "isolation", Value: HCLBool(true)})
		}
		if len(p.PortSecurityMACAddresses) > 0 {
			block.Attributes = append(block.Attributes, Attr{Key: "port_security_mac_addresses", Value: HCLStringList(p.PortSecurityMACAddresses)})
		}

		blocks = append(blocks, block)
	}
	DeduplicateNames(blocks)
	return blocks
}
