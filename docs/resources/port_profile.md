---
page_title: "terrifi_port_profile Resource - Terrifi"
subcategory: ""
description: |-
  Manages a switch port profile on the UniFi controller.
---

# terrifi_port_profile (Resource)

Manages a switch port profile (**Devices > Port Manager > Port Profiles**). A profile sets a port's native (untagged) network, which tagged networks it carries, PoE, Port Mode and port security.

Defaults are locked down: with only `name` set, the profile has **Native VLAN: None** and **Block All** tagged networks, so a port using it carries no traffic.

## Example Usage

### Locked port

```terraform
resource "terrifi_port_profile" "locked" {
  name     = "locked"
  poe_mode = "off"
}
```

### Trunk with no native VLAN

```terraform
resource "terrifi_port_profile" "proxmox_trunk" {
  name               = "proxmox-trunk"
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.pve.id, terrifi_network.servers.id]
  poe_mode           = "off"
  stp_edge_state     = "enabled"
}
```

### Access port

```terraform
resource "terrifi_port_profile" "ilo" {
  name              = "ilo"
  native_network_id = terrifi_network.oob.id
  poe_mode          = "off"
}
```

### Uplink (Allow All)

```terraform
resource "terrifi_port_profile" "uplink" {
  name              = "uplink"
  native_network_id = "<default network id>"
  tagged_vlan_mgmt  = "auto"
  stp_edge_state    = "disabled"
}
```

## Controller behaviour worth knowing

- **Native VLAN None is an empty string.** The controller stores None as `native_networkconf_id = ""`. If the field is missing, it uses the Default network instead. This resource always sends the field, so leaving `native_network_id` unset really means None.
- **Tagged networks are stored as an exclusion list.** `tagged_network_ids` is converted to `excluded_networkconf_ids` over the site's LAN networks (purpose `corporate`, `guest` or `vlan-only`). The controller adds each newly created network to the exclusion list of every existing custom profile, so a trunk never starts carrying a network you did not list.
- **`custom` with nothing tagged becomes `block_all`.** The controller rewrites it, so the resource rejects an empty `tagged_network_ids`.
- **Unknown network IDs are rejected at apply.** The controller would otherwise silently replace an unknown native network with Default.

## Schema

### Required

- `name` (String) — The name of the port profile.

### Optional

- `site` (String) — The site to associate the port profile with. Defaults to the provider site. Changing this forces a new resource.
- `native_network_id` (String) — The network carried untagged. Leave unset for Native VLAN: None.
- `tagged_vlan_mgmt` (String) — One of `block_all` (no tagged networks), `custom` (only `tagged_network_ids`), `auto` (Allow All). Defaults to `block_all`.
- `tagged_network_ids` (Set of String) — Networks carried tagged. Required and non-empty when `tagged_vlan_mgmt = "custom"`, not allowed otherwise. Must not include `native_network_id`.
- `poe_mode` (String) — `auto` or `off`. Defaults to `auto`.
- `isolation` (Boolean) — Port isolation. Defaults to `false`.
- `port_security_mac_addresses` (Set of String) — Lower-case MAC addresses allowed on the port. Setting any turns port security (MAC ID restriction) on.
- `stp_edge_state` (String) — Port Mode: `enabled` is Edge, `disabled` is Infrastructure. Leave unset for the controller default. Profiles cannot store `auto`, and removing the attribute keeps the stored value.

### Read-Only

- `id` (String) — The ID of the port profile.
- `forward` (String) — The forwarding mode the controller derived (`all`, `native`, `customize`, `disabled`).

## Import

```shell
terraform import terrifi_port_profile.trunk <id>
terraform import terrifi_port_profile.trunk <site>:<id>
```

Or generate import blocks for every profile with the [Terrifi CLI](../index.md#cli):

```shell
terrifi generate-imports terrifi_port_profile
```
