```terraform
resource "terrifi_port_profile" "locked" {
  name     = "locked"
  poe_mode = "off"
}

resource "terrifi_port_profile" "ilo" {
  name              = "ilo"
  native_network_id = terrifi_network.oob.id
  poe_mode          = "off"
  stp_edge_state    = "enabled"
}

resource "terrifi_port_profile" "server_trunk" {
  name               = "server-trunk"
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.work_100.id, terrifi_network.work_102.id]
  poe_mode           = "off"
  stp_edge_state     = "enabled"
}

resource "terrifi_device_port_overrides" "core_switch" {
  mac                     = "aa:bb:cc:dd:ee:ff"
  default_port_profile_id = terrifi_port_profile.locked.id

  ports = {
    "2" = { name = "hp-dl380-01-ilo", port_profile_id = terrifi_port_profile.ilo.id }
    "4" = { name = "hp-dl380-01", port_profile_id = terrifi_port_profile.server_trunk.id }
  }
}
