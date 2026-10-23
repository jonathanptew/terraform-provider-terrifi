package provider

// TODO(go-unifi): This file reads and writes a switch's port_overrides without
// the SDK's unifi.DevicePortOverrides. That struct tags native_networkconf_id
// (and most other fields) `omitempty`, so decoding and re-encoding an override
// made in the UI with "Native VLAN: None" (native_networkconf_id = "") turns
// it into an override with no native field, which the controller reads as the
// Default network. Overrides are therefore kept as raw JSON objects and only
// the keys this provider owns are touched.
// Fix needed in SDK: drop `omitempty` from the override's string fields that
// have meaningful empty values.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/ubiquiti-community/go-unifi/unifi"
)

// SwitchPort is one entry of a device's port_table: the live port as the
// switch reports it, with the override (or profile) already applied.
type SwitchPort struct {
	PortIdx         int64    `json:"port_idx"`
	Name            string   `json:"name"`
	Media           string   `json:"media"`
	Up              bool     `json:"up"`
	Speed           int64    `json:"speed"`
	IsUplink        bool     `json:"is_uplink"`
	PoeCaps         int64    `json:"poe_caps"`
	PoeMode         string   `json:"poe_mode"`
	PortProfileID   string   `json:"portconf_id"`
	NativeNetworkID *string  `json:"native_networkconf_id"`
	TaggedVLANMgmt  string   `json:"tagged_vlan_mgmt"`
	ExcludedIDs     []string `json:"excluded_networkconf_ids"`
	Forward         string   `json:"forward"`
	AggregatedBy    any      `json:"aggregated_by"`
}

// SwitchDevice is the part of a stat/device entry the port resources use.
type SwitchDevice struct {
	ID            string                       `json:"_id"`
	MAC           string                       `json:"mac"`
	Type          string                       `json:"type"`
	Model         string                       `json:"model"`
	Name          string                       `json:"name"`
	Adopted       bool                         `json:"adopted"`
	State         int64                        `json:"state"`
	PortTable     []SwitchPort                 `json:"port_table"`
	PortOverrides []map[string]json.RawMessage `json:"port_overrides"`
}

// overridePortIdx returns an override's port_idx, or -1 if it has none.
func overridePortIdx(o map[string]json.RawMessage) int64 {
	var idx int64 = -1
	if raw, ok := o["port_idx"]; ok {
		_ = json.Unmarshal(raw, &idx)
	}
	return idx
}

// overrideString returns a string key of an override, or "".
func overrideString(o map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := o[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// GetSwitchDevice fetches a device by MAC with its port table and raw port
// overrides.
func (c *Client) GetSwitchDevice(ctx context.Context, site, mac string) (*SwitchDevice, error) {
	url := fmt.Sprintf("%s%s/api/s/%s/stat/device/%s", c.BaseURL, c.APIPath, site, strings.ToLower(mac))
	var env struct {
		Meta json.RawMessage `json:"meta"`
		Data []SwitchDevice  `json:"data"`
	}
	if err := c.doV1Request(ctx, http.MethodGet, url, nil, &env); err != nil {
		return nil, err
	}
	if err := checkV1Meta(env.Meta); err != nil {
		return nil, err
	}
	if len(env.Data) == 0 {
		return nil, &unifi.NotFoundError{}
	}
	return &env.Data[0], nil
}

// SetPortOverrides replaces a device's whole port_overrides array. The
// controller has no per-port write, so callers must send every override they
// want to keep.
func (c *Client) SetPortOverrides(ctx context.Context, site, deviceID string, overrides []map[string]json.RawMessage) error {
	if overrides == nil {
		overrides = []map[string]json.RawMessage{}
	}
	url := fmt.Sprintf("%s%s/api/s/%s/rest/device/%s", c.BaseURL, c.APIPath, site, deviceID)
	var env struct {
		Meta json.RawMessage `json:"meta"`
	}
	body := map[string]any{"port_overrides": overrides}
	if err := c.doV1Request(ctx, http.MethodPut, url, body, &env); err != nil {
		return err
	}
	return checkV1Meta(env.Meta)
}

// Device states as reported in stat/device.
const (
	deviceStateConnected = 1
)

// WaitForDeviceConnected polls until the device reports connected again after
// a provision, or the timeout passes. A port change re-provisions the switch.
func (c *Client) WaitForDeviceConnected(ctx context.Context, site, mac string, timeout time.Duration) (*SwitchDevice, error) {
	deadline := time.Now().Add(timeout)
	// Give the controller a moment to move the device out of "connected".
	if err := sleepCtx(ctx, 2*time.Second); err != nil {
		return nil, err
	}
	for {
		// Polling must see fresh data even with response caching on.
		if c.cache != nil {
			c.cache.invalidateAll()
		}
		dev, err := c.GetSwitchDevice(ctx, site, mac)
		if err != nil {
			return nil, err
		}
		if dev.State == deviceStateConnected {
			return dev, nil
		}
		if time.Now().After(deadline) {
			return dev, fmt.Errorf("device %s did not reconnect within %s (state %d)", mac, timeout, dev.State)
		}
		if err := sleepCtx(ctx, 3*time.Second); err != nil {
			return nil, err
		}
	}
}

// ListSwitchDevices returns every adopted device on the site that has switch
// ports (switches, and gateways with a LAN switch).
func (c *Client) ListSwitchDevices(ctx context.Context, site string) ([]SwitchDevice, error) {
	url := fmt.Sprintf("%s%s/api/s/%s/stat/device", c.BaseURL, c.APIPath, site)
	var env struct {
		Meta json.RawMessage `json:"meta"`
		Data []SwitchDevice  `json:"data"`
	}
	if err := c.doV1Request(ctx, http.MethodGet, url, nil, &env); err != nil {
		return nil, err
	}
	if err := checkV1Meta(env.Meta); err != nil {
		return nil, err
	}
	out := env.Data[:0]
	for _, d := range env.Data {
		if d.Adopted && len(d.PortTable) > 0 {
			out = append(out, d)
		}
	}
	return out, nil
}

// PortConfig is one port as terrifi_device_port_overrides would import it.
// Empty strings are unset attributes.
type PortConfig struct {
	Name             string
	PortProfileID    string
	NativeNetworkID  string
	TaggedVLANMgmt   string
	TaggedNetworkIDs []string
	PoeMode          string
	StpEdgeState     string
}

// PortConfigsForImport returns the ports an import of dev would take in,
// keyed by port number.
func PortConfigsForImport(dev *SwitchDevice, switchable []string) map[int64]PortConfig {
	out := map[int64]PortConfig{}
	for _, o := range dev.PortOverrides {
		idx := overridePortIdx(o)
		if idx < 0 {
			continue
		}
		var po portOverrideModel
		readPortOverride(&po, o, switchable, true)
		pc := PortConfig{
			Name:            po.Name.ValueString(),
			PortProfileID:   po.PortProfileID.ValueString(),
			NativeNetworkID: po.NativeNetworkID.ValueString(),
			TaggedVLANMgmt:  po.TaggedVLANMgmt.ValueString(),
			PoeMode:         po.PoeMode.ValueString(),
			StpEdgeState:    po.StpEdgeState.ValueString(),
		}
		for _, v := range po.TaggedNetworkIDs.Elements() {
			if s, ok := v.(types.String); ok {
				pc.TaggedNetworkIDs = append(pc.TaggedNetworkIDs, s.ValueString())
			}
		}
		sort.Strings(pc.TaggedNetworkIDs)
		out[idx] = pc
	}
	return out
}
