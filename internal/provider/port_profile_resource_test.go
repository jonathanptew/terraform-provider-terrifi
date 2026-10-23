package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquiti-community/go-unifi/unifi"
)

// ---------------------------------------------------------------------------
// Unit tests — no TF_ACC, no network, no env vars needed
// ---------------------------------------------------------------------------

func stringSet(vals ...string) types.Set {
	elems := make([]attr.Value, len(vals))
	for i, v := range vals {
		elems[i] = types.StringValue(v)
	}
	return types.SetValueMust(types.StringType, elems)
}

func lockedPortProfileModel(name string) *portProfileResourceModel {
	return &portProfileResourceModel{
		Name:                     types.StringValue(name),
		NativeNetworkID:          types.StringNull(),
		TaggedVLANMgmt:           types.StringValue(taggedVLANBlockAll),
		TaggedNetworkIDs:         types.SetNull(types.StringType),
		PoeMode:                  types.StringValue("auto"),
		Isolation:                types.BoolValue(false),
		PortSecurityMACAddresses: types.SetNull(types.StringType),
		StpEdgeState:             types.StringNull(),
	}
}

func TestPortProfilePayloadJSON(t *testing.T) {
	// The whole point of bypassing the SDK: "" must reach the controller,
	// because an absent native_networkconf_id means the Default network.
	b, err := json.Marshal(PortProfile{Name: "locked", TaggedVLANMgmt: taggedVLANBlockAll})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"native_networkconf_id":""`)
	assert.Contains(t, string(b), `"excluded_networkconf_ids":null`)
	assert.Contains(t, string(b), `"isolation":false`)
	assert.Contains(t, string(b), `"port_security_enabled":false`)
	assert.NotContains(t, string(b), `"forward"`)
	assert.NotContains(t, string(b), `"_id"`)
}

func TestPortProfileModelToAPI(t *testing.T) {
	r := &portProfileResource{}
	ctx := t.Context()
	switchable := []string{"def", "v10", "v20", "v30"}

	t.Run("locked port sends native none", func(t *testing.T) {
		p, diags := r.modelToAPI(ctx, lockedPortProfileModel("locked"), switchable)
		require.False(t, diags.HasError(), diags)
		assert.Equal(t, "locked", p.Name)
		assert.Equal(t, "", p.NativeNetworkID)
		assert.Equal(t, taggedVLANBlockAll, p.TaggedVLANMgmt)
		assert.Nil(t, p.ExcludedNetworkIDs)
		assert.Equal(t, "switch", p.OpMode)
		assert.False(t, p.PortSecurityEnabled)
		assert.Nil(t, p.PortSecurityMACAddress)
	})

	t.Run("access port", func(t *testing.T) {
		m := lockedPortProfileModel("cam")
		m.NativeNetworkID = types.StringValue("v30")
		m.PoeMode = types.StringValue("off")
		m.Isolation = types.BoolValue(true)
		m.StpEdgeState = types.StringValue("enabled")
		p, diags := r.modelToAPI(ctx, m, switchable)
		require.False(t, diags.HasError(), diags)
		assert.Equal(t, "enabled", p.StpEdgeState)
		assert.Equal(t, "v30", p.NativeNetworkID)
		assert.Equal(t, "off", p.PoeMode)
		assert.True(t, p.Isolation)
	})

	t.Run("custom trunk with native none excludes everything not tagged", func(t *testing.T) {
		m := lockedPortProfileModel("trunk")
		m.TaggedVLANMgmt = types.StringValue(taggedVLANCustom)
		m.TaggedNetworkIDs = stringSet("v20", "v10")
		p, diags := r.modelToAPI(ctx, m, switchable)
		require.False(t, diags.HasError(), diags)
		assert.Equal(t, "", p.NativeNetworkID)
		assert.Equal(t, []string{"def", "v30"}, p.ExcludedNetworkIDs)
	})

	t.Run("custom trunk never excludes its native network", func(t *testing.T) {
		m := lockedPortProfileModel("ap")
		m.NativeNetworkID = types.StringValue("def")
		m.TaggedVLANMgmt = types.StringValue(taggedVLANCustom)
		m.TaggedNetworkIDs = stringSet("v10")
		p, diags := r.modelToAPI(ctx, m, switchable)
		require.False(t, diags.HasError(), diags)
		assert.Equal(t, []string{"v20", "v30"}, p.ExcludedNetworkIDs)
	})

	t.Run("mac addresses enable port security, sorted", func(t *testing.T) {
		m := lockedPortProfileModel("mac")
		m.PortSecurityMACAddresses = stringSet("aa:bb:cc:dd:ee:02", "aa:bb:cc:dd:ee:01")
		p, diags := r.modelToAPI(ctx, m, switchable)
		require.False(t, diags.HasError(), diags)
		assert.True(t, p.PortSecurityEnabled)
		assert.Equal(t, []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, p.PortSecurityMACAddress)
	})

	t.Run("empty mac set turns port security off", func(t *testing.T) {
		m := lockedPortProfileModel("mac")
		m.PortSecurityMACAddresses = stringSet()
		p, diags := r.modelToAPI(ctx, m, switchable)
		require.False(t, diags.HasError(), diags)
		assert.False(t, p.PortSecurityEnabled)
		assert.Equal(t, []string{}, p.PortSecurityMACAddress)
	})

	t.Run("unknown native network is an error", func(t *testing.T) {
		m := lockedPortProfileModel("bad")
		m.NativeNetworkID = types.StringValue("nope")
		_, diags := r.modelToAPI(ctx, m, switchable)
		require.True(t, diags.HasError())
		assert.Contains(t, diags.Errors()[0].Summary(), "Unknown native network")
	})

	t.Run("unknown tagged network is an error", func(t *testing.T) {
		m := lockedPortProfileModel("bad")
		m.TaggedVLANMgmt = types.StringValue(taggedVLANCustom)
		m.TaggedNetworkIDs = stringSet("v10", "nope")
		_, diags := r.modelToAPI(ctx, m, switchable)
		require.True(t, diags.HasError())
		assert.Contains(t, diags.Errors()[0].Summary(), "Unknown tagged network")
	})

	t.Run("native network listed as tagged is an error", func(t *testing.T) {
		m := lockedPortProfileModel("bad")
		m.NativeNetworkID = types.StringValue("v10")
		m.TaggedVLANMgmt = types.StringValue(taggedVLANCustom)
		m.TaggedNetworkIDs = stringSet("v10", "v20")
		_, diags := r.modelToAPI(ctx, m, switchable)
		require.True(t, diags.HasError())
		assert.Contains(t, diags.Errors()[0].Summary(), "Native network listed as tagged")
	})
}

func TestPortProfileAPIToModel(t *testing.T) {
	r := &portProfileResource{}
	switchable := []string{"def", "v10", "v20", "v30"}

	t.Run("native none reads back as null", func(t *testing.T) {
		var m portProfileResourceModel
		r.apiToModel(&PortProfile{
			ID: "p1", Name: "locked", NativeNetworkID: "", TaggedVLANMgmt: taggedVLANBlockAll, Forward: "disabled",
		}, &m, "default", switchable)
		assert.Equal(t, "p1", m.ID.ValueString())
		assert.Equal(t, "default", m.Site.ValueString())
		assert.True(t, m.NativeNetworkID.IsNull())
		assert.Equal(t, taggedVLANBlockAll, m.TaggedVLANMgmt.ValueString())
		assert.True(t, m.TaggedNetworkIDs.IsNull())
		assert.Equal(t, "auto", m.PoeMode.ValueString())
		assert.False(t, m.Isolation.ValueBool())
		assert.True(t, m.PortSecurityMACAddresses.IsNull())
		assert.Equal(t, "disabled", m.Forward.ValueString())
		assert.True(t, m.StpEdgeState.IsNull())
	})

	t.Run("missing tagged_vlan_mgmt reads as allow all", func(t *testing.T) {
		var m portProfileResourceModel
		r.apiToModel(&PortProfile{ID: "p2", Name: "ui", NativeNetworkID: "def", Forward: "all"},
			&m, "default", switchable)
		assert.Equal(t, taggedVLANAllowAll, m.TaggedVLANMgmt.ValueString())
		assert.Equal(t, "def", m.NativeNetworkID.ValueString())
		assert.True(t, m.TaggedNetworkIDs.IsNull())
	})

	t.Run("custom derives tagged networks from the exclusion list", func(t *testing.T) {
		var m portProfileResourceModel
		r.apiToModel(&PortProfile{
			ID: "p3", Name: "trunk", TaggedVLANMgmt: taggedVLANCustom, ExcludedNetworkIDs: []string{"def", "v30"},
		}, &m, "default", switchable)
		var tagged []string
		require.False(t, m.TaggedNetworkIDs.ElementsAs(t.Context(), &tagged, false).HasError())
		assert.ElementsMatch(t, []string{"v10", "v20"}, tagged)
	})

	t.Run("network added after the profile shows up as tagged", func(t *testing.T) {
		var m portProfileResourceModel
		r.apiToModel(&PortProfile{
			ID: "p4", Name: "trunk", TaggedVLANMgmt: taggedVLANCustom, ExcludedNetworkIDs: []string{"def", "v30"},
		}, &m, "default", append(switchable, "v40"))
		var tagged []string
		require.False(t, m.TaggedNetworkIDs.ElementsAs(t.Context(), &tagged, false).HasError())
		assert.ElementsMatch(t, []string{"v10", "v20", "v40"}, tagged)
	})

	t.Run("port security disabled hides stale macs", func(t *testing.T) {
		var m portProfileResourceModel
		r.apiToModel(&PortProfile{
			ID: "p5", Name: "x", PortSecurityEnabled: false, PortSecurityMACAddress: []string{"aa:bb:cc:dd:ee:ff"},
		}, &m, "default", switchable)
		assert.True(t, m.PortSecurityMACAddresses.IsNull())
	})

	t.Run("round trip through modelToAPI is stable", func(t *testing.T) {
		in := lockedPortProfileModel("trunk")
		in.NativeNetworkID = types.StringValue("def")
		in.TaggedVLANMgmt = types.StringValue(taggedVLANCustom)
		in.TaggedNetworkIDs = stringSet("v10", "v30")
		in.PortSecurityMACAddresses = stringSet("aa:bb:cc:dd:ee:ff")
		p, diags := r.modelToAPI(t.Context(), in, switchable)
		require.False(t, diags.HasError(), diags)
		p.ID = "p6"

		var out portProfileResourceModel
		r.apiToModel(p, &out, "default", switchable)
		assert.True(t, in.TaggedNetworkIDs.Equal(out.TaggedNetworkIDs))
		assert.True(t, in.NativeNetworkID.Equal(out.NativeNetworkID))
		assert.True(t, in.PortSecurityMACAddresses.Equal(out.PortSecurityMACAddresses))
	})
}

func TestValidatePortProfileTagging(t *testing.T) {
	cases := []struct {
		name    string
		mode    types.String
		tagged  types.Set
		wantErr string
	}{
		{"block_all without list", types.StringValue(taggedVLANBlockAll), types.SetNull(types.StringType), ""},
		{"auto without list", types.StringValue(taggedVLANAllowAll), types.SetNull(types.StringType), ""},
		{"custom with list", types.StringValue(taggedVLANCustom), stringSet("a"), ""},
		{"default mode without list", types.StringNull(), types.SetNull(types.StringType), ""},
		{"unknown mode", types.StringUnknown(), stringSet("a"), ""},
		{"unknown list", types.StringValue(taggedVLANCustom), types.SetUnknown(types.StringType), ""},
		{"custom without list", types.StringValue(taggedVLANCustom), types.SetNull(types.StringType), "Missing tagged networks"},
		{"custom with empty list", types.StringValue(taggedVLANCustom), stringSet(), "Empty tagged networks"},
		{"block_all with list", types.StringValue(taggedVLANBlockAll), stringSet("a"), "Unexpected tagged networks"},
		{"default mode with list", types.StringNull(), stringSet("a"), "Unexpected tagged networks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := validatePortProfileTagging(tc.mode, tc.tagged)
			if tc.wantErr == "" {
				assert.False(t, diags.HasError(), diags)
				return
			}
			require.True(t, diags.HasError())
			assert.Equal(t, tc.wantErr, diags.Errors()[0].Summary())
		})
	}
}

func TestSwitchableNetworkIDsFrom(t *testing.T) {
	ids := switchableNetworkIDsFrom([]unifi.Network{
		{ID: "lan", Purpose: "corporate"},
		{ID: "guest", Purpose: "guest"},
		{ID: "vlan", Purpose: "vlan-only"},
		{ID: "wan", Purpose: "wan"},
		{ID: "vpn", Purpose: "remote-user-vpn"},
		{ID: "s2s", Purpose: "site-vpn"},
	})
	assert.Equal(t, []string{"lan", "guest", "vlan"}, ids)
}

// ---------------------------------------------------------------------------
// Acceptance tests — require TF_ACC=1 and a UniFi controller
// ---------------------------------------------------------------------------

// testAccPortProfileNetworks declares two vlan-only networks the trunk tests
// tag, plus a third that must stay excluded.
func testAccPortProfileNetworks(suffix string, vlan int) string {
	return fmt.Sprintf(`
resource "terrifi_network" "a" {
  name    = "tfacc-pp-a-%[1]s"
  purpose = "vlan-only"
  vlan_id = %[2]d
}

resource "terrifi_network" "b" {
  name    = "tfacc-pp-b-%[1]s"
  purpose = "vlan-only"
  vlan_id = %[3]d
}

resource "terrifi_network" "c" {
  name    = "tfacc-pp-c-%[1]s"
  purpose = "vlan-only"
  vlan_id = %[4]d
}
`, suffix, vlan, vlan+1, vlan+2)
}

func TestAccPortProfile_lockedByDefault(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set")
	}
	preCheck(t)

	name := "tfacc-pp-locked-" + randomSuffix()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name = %q
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "name", name),
					resource.TestCheckNoResourceAttr("terrifi_port_profile.test", "native_network_id"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_vlan_mgmt", "block_all"),
					resource.TestCheckNoResourceAttr("terrifi_port_profile.test", "tagged_network_ids"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "poe_mode", "auto"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "isolation", "false"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "forward", "disabled"),
					testAccCheckPortProfileRawNativeNone("terrifi_port_profile.test"),
				),
			},
			{
				ResourceName:      "terrifi_port_profile.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// testAccCheckPortProfileRawNativeNone asserts the controller stored
// native_networkconf_id as "" (None), not the Default network.
func testAccCheckPortProfileRawNativeNone(name string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("resource %s not found in state", name)
		}
		client, err := NewClient(context.Background(), ClientConfigFromEnv())
		if err != nil {
			return err
		}
		p, err := client.GetPortProfile(context.Background(), client.Site, rs.Primary.ID)
		if err != nil {
			return err
		}
		if p.NativeNetworkID != "" {
			return fmt.Errorf("native_networkconf_id = %q, want \"\" (None)", p.NativeNetworkID)
		}
		return nil
	}
}

func TestAccPortProfile_accessPortLifecycle(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set")
	}
	preCheck(t)

	suffix := randomSuffix()
	vlan := randomVLAN()
	nets := testAccPortProfileNetworks(suffix, vlan)
	name := "tfacc-pp-access-" + suffix

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Access port on network a, PoE off, isolated.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name              = %q
  native_network_id = terrifi_network.a.id
  poe_mode          = "off"
  isolation         = true
  stp_edge_state    = "enabled"
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "stp_edge_state", "enabled"),
					resource.TestCheckResourceAttrPair("terrifi_port_profile.test", "native_network_id", "terrifi_network.a", "id"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_vlan_mgmt", "block_all"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "poe_mode", "off"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "isolation", "true"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "forward", "native"),
				),
			},
			// Move to network b and add a MAC lock.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name                        = %q
  native_network_id           = terrifi_network.b.id
  poe_mode                    = "off"
  isolation                   = true
  port_security_mac_addresses = ["aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02"]
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair("terrifi_port_profile.test", "native_network_id", "terrifi_network.b", "id"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "port_security_mac_addresses.#", "2"),
					resource.TestCheckTypeSetElemAttr("terrifi_port_profile.test", "port_security_mac_addresses.*", "aa:bb:cc:00:00:01"),
					resource.TestCheckTypeSetElemAttr("terrifi_port_profile.test", "port_security_mac_addresses.*", "aa:bb:cc:00:00:02"),
				),
			},
			// Drop one MAC.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name                        = %q
  native_network_id           = terrifi_network.b.id
  poe_mode                    = "off"
  isolation                   = true
  port_security_mac_addresses = ["aa:bb:cc:00:00:02"]
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "port_security_mac_addresses.#", "1"),
					resource.TestCheckTypeSetElemAttr("terrifi_port_profile.test", "port_security_mac_addresses.*", "aa:bb:cc:00:00:02"),
				),
			},
			// Drop native, PoE, isolation and MAC lock: back to a locked port.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name = %q
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr("terrifi_port_profile.test", "native_network_id"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "poe_mode", "auto"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "isolation", "false"),
					resource.TestCheckNoResourceAttr("terrifi_port_profile.test", "port_security_mac_addresses"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "forward", "disabled"),
					testAccCheckPortProfileRawNativeNone("terrifi_port_profile.test"),
				),
			},
		},
	})
}

func TestAccPortProfile_trunkLifecycle(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set")
	}
	preCheck(t)

	suffix := randomSuffix()
	vlan := randomVLAN()
	nets := testAccPortProfileNetworks(suffix, vlan)
	name := "tfacc-pp-trunk-" + suffix

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Proxmox-style trunk: native None, a and b tagged, c excluded.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name               = %q
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.a.id, terrifi_network.b.id]
  poe_mode           = "off"
  stp_edge_state     = "enabled"
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr("terrifi_port_profile.test", "native_network_id"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_vlan_mgmt", "custom"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_network_ids.#", "2"),
					resource.TestCheckTypeSetElemAttrPair("terrifi_port_profile.test", "tagged_network_ids.*", "terrifi_network.a", "id"),
					resource.TestCheckTypeSetElemAttrPair("terrifi_port_profile.test", "tagged_network_ids.*", "terrifi_network.b", "id"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "forward", "customize"),
					testAccCheckPortProfileRawNativeNone("terrifi_port_profile.test"),
				),
			},
			{
				ResourceName:      "terrifi_port_profile.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			// Swap b for c.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name               = %q
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.a.id, terrifi_network.c.id]
  poe_mode           = "off"
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_network_ids.#", "2"),
					resource.TestCheckTypeSetElemAttrPair("terrifi_port_profile.test", "tagged_network_ids.*", "terrifi_network.a", "id"),
					resource.TestCheckTypeSetElemAttrPair("terrifi_port_profile.test", "tagged_network_ids.*", "terrifi_network.c", "id"),
				),
			},
			// AP-style trunk: native a, tagged b only.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name               = %q
  native_network_id  = terrifi_network.a.id
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.b.id]
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair("terrifi_port_profile.test", "native_network_id", "terrifi_network.a", "id"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_network_ids.#", "1"),
					resource.TestCheckTypeSetElemAttrPair("terrifi_port_profile.test", "tagged_network_ids.*", "terrifi_network.b", "id"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "poe_mode", "auto"),
				),
			},
			// Uplink: allow all.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name              = %q
  native_network_id = terrifi_network.a.id
  tagged_vlan_mgmt  = "auto"
  stp_edge_state    = "disabled"
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "stp_edge_state", "disabled"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_vlan_mgmt", "auto"),
					resource.TestCheckNoResourceAttr("terrifi_port_profile.test", "tagged_network_ids"),
				),
			},
			// And back to a locked trunk-less port.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name             = %q
  tagged_vlan_mgmt = "block_all"
}
`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_vlan_mgmt", "block_all"),
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "forward", "disabled"),
				),
			},
		},
	})
}

// TestAccPortProfile_newNetworkStaysExcluded relies on the controller adding
// every new network to the exclusion list of existing custom profiles: a
// trunk must not start carrying a network nobody listed.
func TestAccPortProfile_newNetworkStaysExcluded(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set")
	}
	preCheck(t)

	suffix := randomSuffix()
	vlan := randomVLAN()
	nets := testAccPortProfileNetworks(suffix, vlan)
	profile := fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name               = "tfacc-pp-late-%s"
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.a.id]
}
`, suffix)
	late := fmt.Sprintf(`
resource "terrifi_network" "late" {
  name       = "tfacc-pp-late-%s"
  purpose    = "vlan-only"
  vlan_id    = %d
  depends_on = [terrifi_port_profile.test]
}
`, suffix, vlan+3)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: nets + profile},
			// The step's own empty-plan check proves the profile did not
			// pick up the new network.
			{
				Config: nets + profile + late,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_network_ids.#", "1"),
					resource.TestCheckTypeSetElemAttrPair("terrifi_port_profile.test", "tagged_network_ids.*", "terrifi_network.a", "id"),
				),
			},
			// Tagging it later works like any other network.
			{
				Config: nets + fmt.Sprintf(`
resource "terrifi_network" "late" {
  name    = "tfacc-pp-late-%[1]s"
  purpose = "vlan-only"
  vlan_id = %[2]d
}
`, suffix, vlan+3) + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name               = "tfacc-pp-late-%s"
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.a.id, terrifi_network.late.id]
}
`, suffix),
				Check: resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_network_ids.#", "2"),
			},
		},
	})
}

// TestAccPortProfile_networksCreatedAlongside creates the trunk and an
// untagged network in one apply with no ordering between them, the case the
// controller's own auto-exclusion cannot cover.
func TestAccPortProfile_networksCreatedAlongside(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set")
	}
	preCheck(t)

	suffix := randomSuffix()
	vlan := randomVLAN()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccPortProfileNetworks(suffix, vlan) + fmt.Sprintf(`
resource "terrifi_port_profile" "test" {
  name               = "tfacc-pp-along-%s"
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.a.id]
}
`, suffix),
				Check: resource.TestCheckResourceAttr("terrifi_port_profile.test", "tagged_network_ids.#", "1"),
			},
		},
	})
}

func TestAccPortProfile_validation(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set")
	}
	preCheck(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "terrifi_port_profile" "test" {
  name             = "tfacc-pp-invalid"
  tagged_vlan_mgmt = "custom"
}
`,
				ExpectError: regexp.MustCompile("Missing tagged networks"),
			},
			{
				Config: `
resource "terrifi_port_profile" "test" {
  name               = "tfacc-pp-invalid"
  tagged_network_ids = ["abc"]
}
`,
				ExpectError: regexp.MustCompile("Unexpected tagged networks"),
			},
			{
				Config: `
resource "terrifi_port_profile" "test" {
  name              = "tfacc-pp-invalid"
  native_network_id = "000000000000000000000000"
}
`,
				ExpectError: regexp.MustCompile("Unknown native network"),
			},
			{
				Config: `
resource "terrifi_port_profile" "test" {
  name                        = "tfacc-pp-invalid"
  port_security_mac_addresses = ["AA:BB:CC:DD:EE:FF"]
}
`,
				ExpectError: regexp.MustCompile("lower-case MAC address"),
			},
		},
	})
}
