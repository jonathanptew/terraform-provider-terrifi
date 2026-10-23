# Switch port profiles for a default-deny port layout: every port starts
# locked, and only the profiles below open anything.

resource "terrifi_network" "oob" {
  name    = "OOB"
  purpose = "vlan-only"
  vlan_id = 101
}

resource "terrifi_network" "pve" {
  name    = "PVE-Mgmt"
  purpose = "vlan-only"
  vlan_id = 100
}

resource "terrifi_network" "servers" {
  name    = "Servers"
  purpose = "vlan-only"
  vlan_id = 110
}

# Native VLAN None, Block All tagged, PoE off: a cable in an unplanned port
# reaches nothing.
resource "terrifi_port_profile" "locked" {
  name     = "locked"
  poe_mode = "off"
}

# Hypervisor trunk: no untagged traffic, only the lab VLANs tagged.
resource "terrifi_port_profile" "proxmox_trunk" {
  name               = "proxmox-trunk"
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.pve.id, terrifi_network.servers.id]
  poe_mode           = "off"
  stp_edge_state     = "enabled"
}

# iLO / IPMI access port.
resource "terrifi_port_profile" "ilo" {
  name              = "ilo"
  native_network_id = terrifi_network.oob.id
  poe_mode          = "off"
  stp_edge_state    = "enabled"
}
