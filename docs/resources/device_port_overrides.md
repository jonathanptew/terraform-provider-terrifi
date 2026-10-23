---
page_title: "terrifi_device_port_overrides Resource - Terrifi"
subcategory: ""
description: |-
  Manages the per-port settings of one adopted switch.
---

# terrifi_device_port_overrides (Resource)

Manages the ports of one adopted switch (**Devices > _switch_ > Ports**): each port's name, and either the port profile it uses or the settings made on the port itself (the UI's **Port Profile** toggle off).

Use **one resource per switch**. The controller stores all of a switch's port settings as a single list, so two resources on the same switch would overwrite each other. Ports the resource does not list keep whatever the UI set, unless `default_port_profile_id` is set.

Every change re-provisions the switch. The apply waits (up to 5 minutes) for the switch to reconnect.

## Example Usage

### Profiles by device class, everything else locked

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
```

### Settings on the port itself

```terraform
resource "terrifi_device_port_overrides" "core_switch" {
  mac = "aa:bb:cc:dd:ee:ff"

  ports = {
    "4" = {
      name               = "hp-dl380-01"
      tagged_vlan_mgmt   = "custom" # no native_network_id: Native VLAN None
      tagged_network_ids = [terrifi_network.work_100.id, terrifi_network.work_102.id]
      poe_mode           = "off"
      stp_edge_state     = "enabled" # Port Mode: Edge
    }
  }
}
```

## Bringing hand-configured ports under Terraform

Ports configured in the UI import as they are, with no changes planned:

```shell
terrifi generate-imports terrifi_device_port_overrides > ports.tf
tofu plan   # "1 to import, 0 to change"
```

The generated block lists every port that has settings, with its per-port values (or its profile). After the import, move ports onto profiles one at a time by replacing their settings with `port_profile_id`. Settings this resource does not model (link speed, advanced options) are kept on ports configured inline, and dropped when a port moves to a profile.

## Schema

### Required

- `mac` (String) The switch's MAC address. Changing it replaces the resource.
- `ports` (Map of Object) Managed ports, keyed by port number (`"1"`, `"2"`, ...). See [below](#nested-ports).

### Optional

- `default_port_profile_id` (String) A profile applied to every port not listed in `ports`, except the switch's current uplink. Point it at a locked profile (no native network, `block_all`) so only listed ports carry traffic. If any of those ports drifts off it, the next plan puts it back.
- `site` (String) The site the switch belongs to. Defaults to the provider site.

### Read-Only

- `id` (String) The controller's ID for the switch.

<a id="nested-ports"></a>
### Nested Schema for `ports`

- `name` (String) The port's name. Unset keeps the current name.
- `port_profile_id` (String) The `terrifi_port_profile` the port uses. Conflicts with every setting below.

Settings on the port itself, used when `port_profile_id` is unset:

- `tagged_vlan_mgmt` (String) Required without a profile. `block_all`, `custom` (only `tagged_network_ids`) or `auto` (every network).
- `native_network_id` (String) The untagged network. Unset means **Native VLAN: None**.
- `tagged_network_ids` (Set of String) Tagged networks, required with `custom`. Networks created later stay off the port.
- `poe_mode` (String) `auto`, `off`, `pasv24` or `passthrough`. Unset keeps the current setting.
- `stp_edge_state` (String) Port Mode: `enabled` (Edge), `disabled` (Infrastructure) or `auto`. Unset keeps the current setting.

## Destroying

Destroying the resource (or removing a port from `ports`) removes the port's settings and keeps its name. The port then falls back to the controller default, which is the **Default network with every VLAN tagged**: it opens the port. Point the port at a locked profile before removing it if it should stay closed.

## Import

```shell
terraform import terrifi_device_port_overrides.core_switch aa:bb:cc:dd:ee:ff
terraform import terrifi_device_port_overrides.core_switch site:aa:bb:cc:dd:ee:ff
```

Every port that has settings is imported.
