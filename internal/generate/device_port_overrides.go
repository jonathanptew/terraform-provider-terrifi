package generate

import (
	"fmt"
	"sort"
	"strings"
)

// SwitchPort is one port of a switch as terrifi_device_port_overrides
// declares it. Empty strings are unset attributes.
type SwitchPort struct {
	Name             string
	PortProfileID    string
	NativeNetworkID  string
	TaggedVLANMgmt   string
	TaggedNetworkIDs []string
	PoeMode          string
	StpEdgeState     string
}

// SwitchPorts is one switch and its configured ports, keyed by port number.
type SwitchPorts struct {
	MAC   string
	Name  string
	Ports map[int64]SwitchPort
}

// DevicePortOverridesBlocks generates one import + resource block per switch
// that has configured ports. Hand-configured ports are emitted with their
// per-port settings, so the import plans no changes.
func DevicePortOverridesBlocks(switches []SwitchPorts) []ResourceBlock {
	blocks := make([]ResourceBlock, 0, len(switches))
	for _, sw := range switches {
		if len(sw.Ports) == 0 {
			continue
		}
		name := sw.Name
		if name == "" {
			name = sw.MAC
		}
		block := ResourceBlock{
			ResourceType: "terrifi_device_port_overrides",
			ResourceName: ToTerraformName(name),
			ImportID:     sw.MAC,
			Comment:      fmt.Sprintf("Ports of %s (%s)", name, sw.MAC),
		}
		block.Attributes = append(block.Attributes,
			Attr{Key: "mac", Value: HCLString(sw.MAC)},
			Attr{Key: "ports", Value: hclPorts(sw.Ports)},
		)
		blocks = append(blocks, block)
	}
	DeduplicateNames(blocks)
	return blocks
}

func hclPorts(ports map[int64]SwitchPort) string {
	idxs := make([]int64, 0, len(ports))
	for idx := range ports {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })

	var b strings.Builder
	b.WriteString("{\n")
	for _, idx := range idxs {
		p := ports[idx]
		var attrs []Attr
		add := func(key, value string) {
			if value != "" {
				attrs = append(attrs, Attr{Key: key, Value: HCLString(value)})
			}
		}
		add("name", p.Name)
		if p.PortProfileID != "" {
			add("port_profile_id", p.PortProfileID)
		} else {
			add("native_network_id", p.NativeNetworkID)
			add("tagged_vlan_mgmt", p.TaggedVLANMgmt)
			if len(p.TaggedNetworkIDs) > 0 {
				attrs = append(attrs, Attr{Key: "tagged_network_ids", Value: HCLStringList(p.TaggedNetworkIDs)})
			}
			add("poe_mode", p.PoeMode)
			add("stp_edge_state", p.StpEdgeState)
		}

		width := 0
		for _, a := range attrs {
			width = max(width, len(a.Key))
		}
		fmt.Fprintf(&b, "    %q = {\n", fmt.Sprint(idx))
		for _, a := range attrs {
			fmt.Fprintf(&b, "      %-*s = %s\n", width, a.Key, a.Value)
		}
		b.WriteString("    }\n")
	}
	b.WriteString("  }")
	return b.String()
}
