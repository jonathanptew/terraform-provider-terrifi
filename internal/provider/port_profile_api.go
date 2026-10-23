package provider

// TODO(go-unifi): This file bypasses the go-unifi SDK's PortProfile CRUD
// (rest/portconf). The SDK's unifi.PortProfile tags native_networkconf_id and
// excluded_networkconf_ids with `omitempty`, so two values the controller
// treats as meaningful can never be sent:
//
//  1. native_networkconf_id = "" is how the controller (and its UI, which uses
//     the constant "" for the "None" option) stores "Native VLAN: None".
//     Omitting the field instead makes the controller fall back to the Default
//     network, so a profile meant to carry no untagged traffic silently puts
//     every port on Default.
//  2. excluded_networkconf_ids = [] is needed to clear a previous exclusion
//     list on update; omitting it leaves the old list in place.
//
// Fix needed in SDK: drop `omitempty` from both fields (and make
// TaggedVLANMgmt round-trip). Then this file can use the SDK methods.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ubiquiti-community/go-unifi/unifi"
)

// PortProfile is the rest/portconf object, limited to the fields the
// terrifi_port_profile resource manages plus the read-only ones it reports.
// The JSON tags deliberately differ from the SDK's (see file-level TODO).
type PortProfile struct {
	ID                     string   `json:"_id,omitempty"`
	Name                   string   `json:"name"`
	NativeNetworkID        string   `json:"native_networkconf_id"`
	TaggedVLANMgmt         string   `json:"tagged_vlan_mgmt"`
	ExcludedNetworkIDs     []string `json:"excluded_networkconf_ids"`
	PoeMode                string   `json:"poe_mode,omitempty"`
	OpMode                 string   `json:"op_mode,omitempty"`
	Isolation              bool     `json:"isolation"`
	PortSecurityEnabled    bool     `json:"port_security_enabled"`
	PortSecurityMACAddress []string `json:"port_security_mac_address"`
	StpEdgeState           string   `json:"stp_edge_state,omitempty"`

	// Read-only: the controller derives forward from tagged_vlan_mgmt and the
	// native network, so it is never sent.
	Forward string `json:"forward,omitempty"`
}

// portProfileEnvelope is the v1 response envelope for rest/portconf.
type portProfileEnvelope struct {
	Meta json.RawMessage `json:"meta"`
	Data []PortProfile   `json:"data"`
}

func (c *Client) portProfileURL(site, id string) string {
	url := fmt.Sprintf("%s%s/api/s/%s/rest/portconf", c.BaseURL, c.APIPath, site)
	if id != "" {
		url += "/" + id
	}
	return url
}

// sendPortProfile issues a POST or PUT and returns the stored profile. The
// controller answers both with the stored object, which already reflects its
// normalizations (e.g. custom with nothing tagged becomes block_all).
func (c *Client) sendPortProfile(ctx context.Context, method, site string, p *PortProfile) (*PortProfile, error) {
	body := *p
	body.Forward = ""
	if body.ExcludedNetworkIDs == nil {
		body.ExcludedNetworkIDs = []string{}
	}
	if body.PortSecurityMACAddress == nil {
		body.PortSecurityMACAddress = []string{}
	}

	id := ""
	if method == http.MethodPut {
		id = p.ID
	}

	var env portProfileEnvelope
	if err := c.doV1Request(ctx, method, c.portProfileURL(site, id), body, &env); err != nil {
		return nil, err
	}
	if err := checkV1Meta(env.Meta); err != nil {
		return nil, err
	}
	if len(env.Data) == 0 && method == http.MethodPut {
		return c.GetPortProfile(ctx, site, p.ID)
	}
	if len(env.Data) != 1 {
		return nil, fmt.Errorf("expected 1 port profile in response, got %d", len(env.Data))
	}
	return &env.Data[0], nil
}

// CreatePortProfile creates a port profile (rest/portconf).
func (c *Client) CreatePortProfile(ctx context.Context, site string, p *PortProfile) (*PortProfile, error) {
	return c.sendPortProfile(ctx, http.MethodPost, site, p)
}

// UpdatePortProfile updates a port profile. The v1 PUT merges the sent fields
// into the stored object, so fields this resource does not manage are kept.
func (c *Client) UpdatePortProfile(ctx context.Context, site string, p *PortProfile) (*PortProfile, error) {
	return c.sendPortProfile(ctx, http.MethodPut, site, p)
}

// GetPortProfile reads one port profile by ID.
func (c *Client) GetPortProfile(ctx context.Context, site, id string) (*PortProfile, error) {
	var env portProfileEnvelope
	if err := c.doV1Request(ctx, http.MethodGet, c.portProfileURL(site, id), nil, &env); err != nil {
		return nil, err
	}
	if err := checkV1Meta(env.Meta); err != nil {
		return nil, err
	}
	if len(env.Data) != 1 {
		return nil, &unifi.NotFoundError{}
	}
	return &env.Data[0], nil
}

// ListPortProfiles lists every port profile on the site.
func (c *Client) ListPortProfiles(ctx context.Context, site string) ([]PortProfile, error) {
	var env portProfileEnvelope
	if err := c.doV1Request(ctx, http.MethodGet, c.portProfileURL(site, ""), nil, &env); err != nil {
		return nil, err
	}
	if err := checkV1Meta(env.Meta); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// DeletePortProfile deletes a port profile.
func (c *Client) DeletePortProfile(ctx context.Context, site, id string) error {
	var env portProfileEnvelope
	if err := c.doV1Request(ctx, http.MethodDelete, c.portProfileURL(site, id), struct{}{}, &env); err != nil {
		return err
	}
	return checkV1Meta(env.Meta)
}

// SwitchableNetworkIDs returns the IDs of the networks a switch port can carry
// as tagged VLANs: every LAN-side network (corporate, guest, vlan-only). The
// controller stores trunks as an exclusion list over this set, so the
// resource converts between "tagged" and "excluded" using it.
func (c *Client) SwitchableNetworkIDs(ctx context.Context, site string) ([]string, error) {
	networks, err := c.ListNetwork(ctx, site)
	if err != nil {
		return nil, err
	}
	return switchableNetworkIDsFrom(networks), nil
}

func switchableNetworkIDsFrom(networks []unifi.Network) []string {
	ids := make([]string, 0, len(networks))
	for _, n := range networks {
		switch n.Purpose {
		case "corporate", "guest", "vlan-only":
			ids = append(ids, n.ID)
		}
	}
	return ids
}
