package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Unit tests — no TF_ACC, no network, no env vars needed
// ---------------------------------------------------------------------------

func rawOverride(t *testing.T, js string) map[string]json.RawMessage {
	t.Helper()
	var o map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(js), &o))
	return o
}

func overrideJSON(t *testing.T, o map[string]json.RawMessage) map[string]any {
	t.Helper()
	b, err := json.Marshal(o)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func inlinePort(native, mode string, tagged ...string) portOverrideModel {
	po := portOverrideModel{
		Name:             types.StringNull(),
		PortProfileID:    types.StringNull(),
		NativeNetworkID:  optionalString(native),
		TaggedVLANMgmt:   types.StringValue(mode),
		TaggedNetworkIDs: types.SetNull(types.StringType),
		PoeMode:          types.StringNull(),
		StpEdgeState:     types.StringNull(),
	}
	if len(tagged) > 0 {
		po.TaggedNetworkIDs = stringSet(tagged...)
	}
	return po
}

func profilePort(id string) portOverrideModel {
	return portOverrideModel{
		Name:             types.StringNull(),
		PortProfileID:    types.StringValue(id),
		NativeNetworkID:  types.StringNull(),
		TaggedVLANMgmt:   types.StringNull(),
		TaggedNetworkIDs: types.SetNull(types.StringType),
		PoeMode:          types.StringNull(),
		StpEdgeState:     types.StringNull(),
	}
}

func testSwitch(t *testing.T, overrides ...string) *SwitchDevice {
	dev := &SwitchDevice{ID: "dev1", MAC: "00:27:22:00:00:01"}
	for i := int64(1); i <= 6; i++ {
		dev.PortTable = append(dev.PortTable, SwitchPort{PortIdx: i, IsUplink: i == 1})
	}
	for _, o := range overrides {
		dev.PortOverrides = append(dev.PortOverrides, rawOverride(t, o))
	}
	return dev
}

func byIdx(t *testing.T, out []map[string]json.RawMessage) map[int64]map[string]any {
	m := map[int64]map[string]any{}
	for _, o := range out {
		m[overridePortIdx(o)] = overrideJSON(t, o)
	}
	return m
}

var testSwitchable = []string{"def", "oob", "w100", "w102"}

func TestBuildPortOverrides_inlineKeepsManualSettings(t *testing.T) {
	dev := testSwitch(t,
		`{"port_idx":2,"name":"ilo","native_networkconf_id":"def","forward":"all","setting_preference":"manual","speed":1000,"autoneg":false}`,
		`{"port_idx":5,"name":"ui-only","portconf_id":"p9"}`,
	)
	ilo := inlinePort("oob", taggedVLANBlockAll)
	ilo.PoeMode = types.StringValue("off")
	ilo.StpEdgeState = types.StringValue("enabled")
	plan := &devicePortOverridesModel{
		MAC:                  types.StringValue(dev.MAC),
		DefaultPortProfileID: types.StringNull(),
		Ports: map[string]portOverrideModel{
			"2": ilo,
			"4": inlinePort("", taggedVLANCustom, "w100", "w102"),
		},
	}

	out, diags := buildPortOverrides(context.Background(), dev, plan, nil, testSwitchable)
	require.False(t, diags.HasError(), "%v", diags)
	got := byIdx(t, out)
	require.Len(t, got, 3)

	assert.Equal(t, map[string]any{
		"port_idx": 2.0, "name": "ilo", "native_networkconf_id": "oob", "tagged_vlan_mgmt": "block_all",
		"excluded_networkconf_ids": []any{}, "forward": "native", "poe_mode": "off", "stp_edge_state": "enabled",
		"setting_preference": "manual", "speed": 1000.0, "autoneg": false,
	}, got[2])
	assert.Equal(t, map[string]any{
		"port_idx": 4.0, "native_networkconf_id": "", "tagged_vlan_mgmt": "custom",
		"excluded_networkconf_ids": []any{"def", "oob"}, "forward": "customize",
	}, got[4])
	// Unmanaged port passed through untouched.
	assert.Equal(t, map[string]any{"port_idx": 5.0, "name": "ui-only", "portconf_id": "p9"}, got[5])
}

func TestBuildPortOverrides_defaultProfileAndRelease(t *testing.T) {
	dev := testSwitch(t,
		`{"port_idx":2,"name":"old","portconf_id":"trunk"}`,
		`{"port_idx":3,"name":"cam","native_networkconf_id":"","tagged_vlan_mgmt":"block_all"}`,
	)
	prior := &devicePortOverridesModel{
		Ports:                map[string]portOverrideModel{"2": profilePort("trunk")},
		DefaultPortProfileID: types.StringNull(),
	}
	plan := &devicePortOverridesModel{
		MAC:                  types.StringValue(dev.MAC),
		DefaultPortProfileID: types.StringValue("locked"),
		Ports:                map[string]portOverrideModel{"4": profilePort("trunk")},
	}

	out, diags := buildPortOverrides(context.Background(), dev, plan, prior, testSwitchable)
	require.False(t, diags.HasError(), "%v", diags)
	got := byIdx(t, out)

	assert.NotContains(t, got, int64(1), "uplink must not get the default profile")
	assert.Equal(t, map[string]any{"port_idx": 2.0, "name": "old", "portconf_id": "locked"}, got[2])
	assert.Equal(t, map[string]any{"port_idx": 3.0, "name": "cam", "portconf_id": "locked"}, got[3])
	assert.Equal(t, map[string]any{"port_idx": 4.0, "portconf_id": "trunk"}, got[4])
	assert.Equal(t, map[string]any{"port_idx": 6.0, "portconf_id": "locked"}, got[6])

	// Destroy: everything this resource owned is released, names kept.
	empty := &devicePortOverridesModel{MAC: plan.MAC, DefaultPortProfileID: types.StringNull()}
	dev.PortOverrides = out
	out, diags = buildPortOverrides(context.Background(), dev, empty, plan, testSwitchable)
	require.False(t, diags.HasError(), "%v", diags)
	got = byIdx(t, out)
	assert.Equal(t, map[int64]map[string]any{
		2: {"port_idx": 2.0, "name": "old"},
		3: {"port_idx": 3.0, "name": "cam"},
	}, got)
}

func TestBuildPortOverrides_errors(t *testing.T) {
	dev := testSwitch(t)
	plan := &devicePortOverridesModel{
		MAC:                  types.StringValue(dev.MAC),
		DefaultPortProfileID: types.StringNull(),
		Ports: map[string]portOverrideModel{
			"9": profilePort("p"),
		},
	}
	_, diags := buildPortOverrides(context.Background(), dev, plan, nil, testSwitchable)
	require.True(t, diags.HasError())
	assert.Contains(t, diags[0].Detail(), "has no port 9")

	plan.Ports = map[string]portOverrideModel{"2": inlinePort("oob", taggedVLANCustom, "oob", "nope")}
	_, diags = buildPortOverrides(context.Background(), dev, plan, nil, testSwitchable)
	require.Equal(t, 2, diags.ErrorsCount(), "%v", diags)

	plan.Ports = map[string]portOverrideModel{"1": profilePort("p")}
	_, diags = buildPortOverrides(context.Background(), dev, plan, nil, testSwitchable)
	require.False(t, diags.HasError())
	assert.Equal(t, 1, diags.WarningsCount(), "uplink warning")
}

func TestDeviceToModel_importsManualPorts(t *testing.T) {
	// Overrides as the controller stores ports set by hand in the UI.
	dev := testSwitch(t,
		`{"port_idx":2,"name":"hp-dl380-01-ilo","native_networkconf_id":"oob","tagged_vlan_mgmt":"block_all","poe_mode":"off","stp_edge_state":"enabled","setting_preference":"manual","voice_networkconf_id":"","forward":"native"}`,
		`{"port_idx":4,"name":"hp-dl380-01","native_networkconf_id":"","tagged_vlan_mgmt":"custom","excluded_networkconf_ids":["def","oob"],"poe_mode":"off","stp_edge_state":"enabled","forward":"customize"}`,
		`{"port_idx":5,"portconf_id":"p9"}`,
		`{"port_idx":6,"name":"legacy","native_networkconf_id":"def","forward":"all"}`,
	)
	r := &devicePortOverridesResource{}
	var m devicePortOverridesModel
	r.deviceToModel(dev, &m, "default", testSwitchable)

	require.Len(t, m.Ports, 4)
	ilo := m.Ports["2"]
	assert.Equal(t, "hp-dl380-01-ilo", ilo.Name.ValueString())
	assert.True(t, ilo.PortProfileID.IsNull())
	assert.Equal(t, "oob", ilo.NativeNetworkID.ValueString())
	assert.Equal(t, "block_all", ilo.TaggedVLANMgmt.ValueString())
	assert.True(t, ilo.TaggedNetworkIDs.IsNull())
	assert.Equal(t, "off", ilo.PoeMode.ValueString())
	assert.Equal(t, "enabled", ilo.StpEdgeState.ValueString())

	trunk := m.Ports["4"]
	assert.True(t, trunk.NativeNetworkID.IsNull(), "native None")
	assert.Equal(t, "custom", trunk.TaggedVLANMgmt.ValueString())
	assert.Equal(t, stringSet("w100", "w102"), trunk.TaggedNetworkIDs)

	assert.Equal(t, profilePort("p9"), m.Ports["5"])
	assert.Equal(t, "auto", m.Ports["6"].TaggedVLANMgmt.ValueString(), "forward fallback")
	assert.True(t, m.DefaultPortProfileID.IsNull())

	// Writing the imported model back changes nothing the controller cares about.
	out, diags := buildPortOverrides(context.Background(), dev, &m, &m, testSwitchable)
	require.False(t, diags.HasError(), "%v", diags)
	dev2 := *dev
	dev2.PortOverrides = out
	var m2 devicePortOverridesModel
	r.deviceToModel(&dev2, &m2, "default", testSwitchable)
	assert.Equal(t, m.Ports, m2.Ports)
}

func TestDeviceToModel_drift(t *testing.T) {
	dev := testSwitch(t,
		`{"port_idx":2,"native_networkconf_id":"def","forward":"all"}`,
		`{"port_idx":3,"portconf_id":"locked"}`,
	)
	m := devicePortOverridesModel{
		DefaultPortProfileID: types.StringValue("locked"),
		Ports: map[string]portOverrideModel{
			"2": profilePort("trunk"),
		},
	}
	(&devicePortOverridesResource{}).deviceToModel(dev, &m, "default", testSwitchable)
	assert.True(t, m.Ports["2"].PortProfileID.IsNull(), "port moved off its profile in the UI")
	assert.True(t, m.Ports["2"].TaggedVLANMgmt.IsNull(), "profile ports keep per-port settings null")
	assert.True(t, m.DefaultPortProfileID.IsNull(), "ports 4-6 lack the default profile")
}

func TestValidatePortOverride(t *testing.T) {
	base := path.Root("ports").AtMapKey("2")

	assert.False(t, validatePortOverride(base, profilePort("p")).HasError())
	assert.False(t, validatePortOverride(base, inlinePort("", taggedVLANBlockAll)).HasError())
	assert.False(t, validatePortOverride(base, inlinePort("", taggedVLANCustom, "a")).HasError())

	both := profilePort("p")
	both.PoeMode = types.StringValue("off")
	assert.True(t, validatePortOverride(base, both).HasError())

	neither := profilePort("p")
	neither.PortProfileID = types.StringNull()
	assert.True(t, validatePortOverride(base, neither).HasError())

	assert.True(t, validatePortOverride(base, inlinePort("", taggedVLANCustom)).HasError())
	assert.True(t, validatePortOverride(base, inlinePort("", taggedVLANBlockAll, "a")).HasError())
}

// ---------------------------------------------------------------------------
// Acceptance tests — need TF_ACC and a controller. The Docker controller runs
// in simulation mode with switches that can be adopted; these tests share
// testAccSwitchMAC, so they are not parallel.
// ---------------------------------------------------------------------------

const testAccSwitchMAC = "00:27:22:00:00:01"

// testAccAdoptSwitch adopts the simulated switch if needed and waits for it.
func testAccAdoptSwitch(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	client, err := NewClient(ctx, ClientConfigFromEnv())
	require.NoError(t, err)

	dev, err := client.GetSwitchDevice(ctx, client.Site, testAccSwitchMAC)
	if err != nil {
		t.Skipf("no simulated switch %s on this controller: %v", testAccSwitchMAC, err)
	}
	if !dev.Adopted {
		url := fmt.Sprintf("%s%s/api/s/%s/cmd/devmgr", client.BaseURL, client.APIPath, client.Site)
		body := map[string]string{"cmd": "adopt", "mac": testAccSwitchMAC}
		var env struct {
			Meta json.RawMessage `json:"meta"`
		}
		require.NoError(t, client.doV1Request(ctx, http.MethodPost, url, body, &env))
		require.NoError(t, checkV1Meta(env.Meta))
	}
	_, err = client.WaitForDeviceConnected(ctx, client.Site, testAccSwitchMAC, 3*time.Minute)
	require.NoError(t, err)
}

// testAccSetManualPorts writes overrides the way the UI does when ports are
// configured by hand (Port Profile toggle off): port 2 an iLO access port,
// port 4 a trunk with native None.
func testAccSetManualPorts(oob, a, b string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		ids := map[string]string{}
		for _, name := range []string{oob, a, b} {
			rs, ok := s.RootModule().Resources[name]
			if !ok {
				return fmt.Errorf("resource %s not found in state", name)
			}
			ids[name] = rs.Primary.ID
		}
		ctx := context.Background()
		client, err := NewClient(ctx, ClientConfigFromEnv())
		if err != nil {
			return err
		}
		switchable, err := client.SwitchableNetworkIDs(ctx, client.Site)
		if err != nil {
			return err
		}
		dev, err := client.GetSwitchDevice(ctx, client.Site, testAccSwitchMAC)
		if err != nil {
			return err
		}
		overrides := []map[string]json.RawMessage{
			{
				"port_idx":              mustJSON(2),
				"name":                  mustJSON("tfacc-ilo"),
				"native_networkconf_id": mustJSON(ids[oob]),
				"tagged_vlan_mgmt":      mustJSON("block_all"),
				"poe_mode":              mustJSON("off"),
				"stp_edge_state":        mustJSON("enabled"),
				"setting_preference":    mustJSON("manual"),
				"speed":                 mustJSON(1000),
				"full_duplex":           mustJSON(true),
				"autoneg":               mustJSON(false),
			},
			{
				"port_idx":                 mustJSON(4),
				"name":                     mustJSON("tfacc-trunk"),
				"native_networkconf_id":    mustJSON(""),
				"tagged_vlan_mgmt":         mustJSON("custom"),
				"excluded_networkconf_ids": mustJSON(excludedFromTagged(switchable, []string{ids[a], ids[b]}, "")),
				"poe_mode":                 mustJSON("off"),
				"stp_edge_state":           mustJSON("enabled"),
			},
		}
		if err := client.SetPortOverrides(ctx, client.Site, dev.ID, overrides); err != nil {
			return err
		}
		_, err = client.WaitForDeviceConnected(ctx, client.Site, testAccSwitchMAC, 3*time.Minute)
		return err
	}
}

// testAccCheckRawOverride asserts a key of a port's stored override.
func testAccCheckRawOverride(port int64, key, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		ctx := context.Background()
		client, err := NewClient(ctx, ClientConfigFromEnv())
		if err != nil {
			return err
		}
		dev, err := client.GetSwitchDevice(ctx, client.Site, testAccSwitchMAC)
		if err != nil {
			return err
		}
		for _, o := range dev.PortOverrides {
			if overridePortIdx(o) == port {
				got := string(o[key])
				if got != want {
					return fmt.Errorf("port %d %s = %s, want %s", port, key, got, want)
				}
				return nil
			}
		}
		if want == "" {
			return nil
		}
		return fmt.Errorf("port %d has no override", port)
	}
}

func testAccPortOverridesNetworks(suffix string, vlan int) string {
	return fmt.Sprintf(`
resource "terrifi_network" "oob" {
  name    = "tfacc-po-oob-%[1]s"
  purpose = "vlan-only"
  vlan_id = %[2]d
}

resource "terrifi_network" "a" {
  name    = "tfacc-po-a-%[1]s"
  purpose = "vlan-only"
  vlan_id = %[3]d
}

resource "terrifi_network" "b" {
  name    = "tfacc-po-b-%[1]s"
  purpose = "vlan-only"
  vlan_id = %[4]d
}
`, suffix, vlan, vlan+1, vlan+2)
}

// TestAccDevicePortOverrides_importManual is the migration path: ports set up
// by hand are imported with no changes, then moved onto profiles.
func TestAccDevicePortOverrides_importManual(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set")
	}
	preCheck(t)
	testAccAdoptSwitch(t)

	suffix := randomSuffix()
	nets := testAccPortOverridesNetworks(suffix, randomVLAN())
	const addr = "terrifi_device_port_overrides.sw"

	manual := nets + fmt.Sprintf(`
import {
  to = terrifi_device_port_overrides.sw
  id = %[1]q
}

resource "terrifi_device_port_overrides" "sw" {
  mac = %[1]q
  ports = {
    "2" = {
      name              = "tfacc-ilo"
      native_network_id = terrifi_network.oob.id
      tagged_vlan_mgmt  = "block_all"
      poe_mode          = "off"
      stp_edge_state    = "enabled"
    }
    "4" = {
      name               = "tfacc-trunk"
      tagged_vlan_mgmt   = "custom"
      tagged_network_ids = [terrifi_network.a.id, terrifi_network.b.id]
      poe_mode           = "off"
      stp_edge_state     = "enabled"
    }
  }
}
`, testAccSwitchMAC)

	profiles := nets + fmt.Sprintf(`
resource "terrifi_port_profile" "locked" {
  name = "tfacc-po-locked-%[2]s"
}

resource "terrifi_port_profile" "trunk" {
  name               = "tfacc-po-trunk-%[2]s"
  tagged_vlan_mgmt   = "custom"
  tagged_network_ids = [terrifi_network.a.id, terrifi_network.b.id]
  poe_mode           = "off"
  stp_edge_state     = "enabled"
}

resource "terrifi_device_port_overrides" "sw" {
  mac                     = %[1]q
  default_port_profile_id = terrifi_port_profile.locked.id
  ports = {
    "2" = {
      name              = "tfacc-ilo"
      native_network_id = terrifi_network.oob.id
      tagged_vlan_mgmt  = "block_all"
      poe_mode          = "off"
      stp_edge_state    = "enabled"
    }
    "4" = {
      name            = "tfacc-trunk"
      port_profile_id = terrifi_port_profile.trunk.id
    }
  }
}
`, testAccSwitchMAC, suffix)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Networks exist; ports configured by hand outside Terraform.
			{
				Config: nets,
				Check:  testAccSetManualPorts("terrifi_network.oob", "terrifi_network.a", "terrifi_network.b"),
			},
			// Import matches the manual config exactly: no changes planned.
			{
				Config: manual,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionNoop),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "ports.%", "2"),
					resource.TestCheckNoResourceAttr(addr, "ports.4.native_network_id"),
					resource.TestCheckResourceAttr(addr, "ports.4.tagged_network_ids.#", "2"),
				),
			},
			// Plain import of the same state verifies too.
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateId:     testAccSwitchMAC,
				ImportStateVerify: true,
			},
			// Move the trunk onto a profile and lock every other port down.
			{
				Config: profiles,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair(addr, "ports.4.port_profile_id", "terrifi_port_profile.trunk", "id"),
					resource.TestCheckResourceAttrPair(addr, "default_port_profile_id", "terrifi_port_profile.locked", "id"),
					testAccCheckRawOverride(4, "native_networkconf_id", ""),
					// The manual link settings on port 2 survive the rewrite.
					testAccCheckRawOverride(2, "speed", "1000"),
					resource.TestCheckResourceAttrPair(addr, "ports.2.native_network_id", "terrifi_network.oob", "id"),
				),
			},
		},
	})
}

func TestAccDevicePortOverrides_validation(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set")
	}
	preCheck(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "terrifi_device_port_overrides" "sw" {
  mac   = %q
  ports = { "2" = { port_profile_id = "x", poe_mode = "off" } }
}
`, testAccSwitchMAC),
				ExpectError: regexp.MustCompile(`Conflicting port settings`),
			},
			{
				Config: fmt.Sprintf(`
resource "terrifi_device_port_overrides" "sw" {
  mac   = %q
  ports = { "port2" = { port_profile_id = "x" } }
}
`, testAccSwitchMAC),
				ExpectError: regexp.MustCompile(`must be a port number`),
			},
		},
	})
}

// Ports built with for_each over resources not yet created reach
// ValidateConfig as an unknown map; validation must wait instead of failing.
func TestDevicePortOverridesValidateConfig_unknownPorts(t *testing.T) {
	ctx := context.Background()
	r := &devicePortOverridesResource{}
	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)
	s := schemaResp.Schema
	typ := s.Type().TerraformType(ctx).(tftypes.Object)

	vals := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	vals["mac"] = tftypes.NewValue(tftypes.String, "00:27:22:00:00:01")
	vals["ports"] = tftypes.NewValue(typ.AttributeTypes["ports"], tftypes.UnknownValue)

	req := fwresource.ValidateConfigRequest{Config: tfsdk.Config{Schema: s, Raw: tftypes.NewValue(typ, vals)}}
	var resp fwresource.ValidateConfigResponse
	r.ValidateConfig(ctx, req, &resp)
	assert.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
}
