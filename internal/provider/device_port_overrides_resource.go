package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/ubiquiti-community/go-unifi/unifi"
)

// Compile-time interface checks.
var (
	_ resource.Resource                   = &devicePortOverridesResource{}
	_ resource.ResourceWithImportState    = &devicePortOverridesResource{}
	_ resource.ResourceWithValidateConfig = &devicePortOverridesResource{}
)

// devicePortProvisionTimeout bounds the wait for a switch to come back after a
// port change re-provisions it.
const devicePortProvisionTimeout = 5 * time.Minute

// NewDevicePortOverridesResource is the factory function registered in provider.Resources().
func NewDevicePortOverridesResource() resource.Resource {
	return &devicePortOverridesResource{}
}

type devicePortOverridesResource struct {
	client *Client
}

type devicePortOverridesModel struct {
	ID                   types.String                 `tfsdk:"id"`
	Site                 types.String                 `tfsdk:"site"`
	MAC                  types.String                 `tfsdk:"mac"`
	Ports                map[string]portOverrideModel `tfsdk:"ports"`
	DefaultPortProfileID types.String                 `tfsdk:"default_port_profile_id"`
}

type portOverrideModel struct {
	Name             types.String `tfsdk:"name"`
	PortProfileID    types.String `tfsdk:"port_profile_id"`
	NativeNetworkID  types.String `tfsdk:"native_network_id"`
	TaggedVLANMgmt   types.String `tfsdk:"tagged_vlan_mgmt"`
	TaggedNetworkIDs types.Set    `tfsdk:"tagged_network_ids"`
	PoeMode          types.String `tfsdk:"poe_mode"`
	StpEdgeState     types.String `tfsdk:"stp_edge_state"`
}

// isInline reports whether the port is configured on the port itself (the UI's
// "Port Profile" toggle off) rather than through a profile.
func (po portOverrideModel) isInline() bool {
	return po.PortProfileID.IsNull()
}

var portIdxRegexp = regexp.MustCompile(`^[1-9][0-9]*$`)

// Metadata sets the resource type name.
func (r *devicePortOverridesResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_device_port_overrides"
}

// Schema defines the HCL schema for the terrifi_device_port_overrides resource.
func (r *devicePortOverridesResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Assigns port profiles (and names) to the ports of one adopted switch. " +
			"Use exactly one of these per switch: the controller stores all of a device's port " +
			"overrides as one list, so two resources on the same switch would overwrite each other. " +
			"Ports this resource does not manage keep whatever the UI set.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The controller ID of the device.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			"site": schema.StringAttribute{
				MarkdownDescription: "The site the device belongs to. Defaults to the provider site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			"mac": schema.StringAttribute{
				MarkdownDescription: "The MAC address of the switch (e.g. `aa:bb:cc:dd:ee:ff`).",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.RegexMatches(macRegexp, "must be a valid MAC address (e.g. aa:bb:cc:dd:ee:ff)"),
				},
			},

			"ports": schema.MapNestedAttribute{
				MarkdownDescription: "Managed ports, keyed by port number (`\"1\"`, `\"2\"`, ...).",
				Required:            true,
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.RegexMatches(portIdxRegexp, "must be a port number, e.g. \"4\"")),
				},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							MarkdownDescription: "The port's name. Leave unset to keep the current name.",
							Optional:            true,
							Validators: []validator.String{
								stringvalidator.LengthBetween(1, 128),
							},
						},
						"port_profile_id": schema.StringAttribute{
							MarkdownDescription: "The `terrifi_port_profile` applied to the port. Any settings made " +
								"on the port itself in the UI are replaced by the profile. Conflicts with the " +
								"per-port settings below.",
							Optional: true,
						},
						"native_network_id": schema.StringAttribute{
							MarkdownDescription: "Per-port setting: the untagged network. Leave unset for Native " +
								"VLAN: None.",
							Optional: true,
						},
						"tagged_vlan_mgmt": schema.StringAttribute{
							MarkdownDescription: "Per-port setting, required when `port_profile_id` is unset: " +
								"`block_all`, `custom` (only `tagged_network_ids`), or `auto` (every network).",
							Optional: true,
							Validators: []validator.String{
								stringvalidator.OneOf(taggedVLANBlockAll, taggedVLANCustom, taggedVLANAllowAll),
							},
						},
						"tagged_network_ids": schema.SetAttribute{
							MarkdownDescription: "Per-port setting: the tagged networks when `tagged_vlan_mgmt` is " +
								"`custom`. Networks created later stay off the port.",
							Optional:    true,
							ElementType: types.StringType,
						},
						"poe_mode": schema.StringAttribute{
							MarkdownDescription: "Per-port setting: `auto`, `off`, `pasv24` or `passthrough`. " +
								"Leave unset to keep the port's current PoE setting.",
							Optional: true,
							Validators: []validator.String{
								stringvalidator.OneOf("auto", "off", "pasv24", "passthrough"),
							},
						},
						"stp_edge_state": schema.StringAttribute{
							MarkdownDescription: "Per-port setting: the UI's Port Mode. `enabled` is Edge, " +
								"`disabled` is Infrastructure, `auto` lets the switch decide. Leave unset to keep " +
								"the current value.",
							Optional: true,
							Validators: []validator.String{
								stringvalidator.OneOf("auto", "enabled", "disabled"),
							},
						},
					},
				},
			},

			"default_port_profile_id": schema.StringAttribute{
				MarkdownDescription: "A profile applied to every port not listed in `ports`, except the " +
					"switch's current uplink. Point this at a locked-down profile (no native network, " +
					"`block_all`) to make the switch deny-by-default: only listed ports carry traffic. " +
					"If a port drifts away from it, the attribute plans as a change so the next apply " +
					"puts it back.",
				Optional: true,
			},
		},
	}
}

// Configure is called by the framework to inject the provider's API client.
func (r *devicePortOverridesResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *Client, got: %T.", req.ProviderData),
		)
		return
	}

	r.client = client
}

// Create applies the managed ports to the switch.
func (r *devicePortOverridesResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var plan devicePortOverridesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	site := r.client.SiteOrDefault(plan.Site)
	dev, switchable, diags := r.apply(ctx, site, &plan, nil)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.deviceToModel(dev, &plan, site, switchable)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the managed ports from the switch.
func (r *devicePortOverridesResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state devicePortOverridesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	site := r.client.SiteOrDefault(state.Site)
	dev, err := r.client.GetSwitchDevice(ctx, site, state.MAC.ValueString())
	if err != nil {
		if _, ok := err.(*unifi.NotFoundError); ok {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Error Reading Device", err.Error())
		return
	}

	switchable, err := r.client.SwitchableNetworkIDs(ctx, site)
	if err != nil {
		resp.Diagnostics.AddError("Error Listing Networks", err.Error())
		return
	}

	r.deviceToModel(dev, &state, site, switchable)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update applies the new set of managed ports, releasing ports no longer listed.
func (r *devicePortOverridesResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var state, plan devicePortOverridesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	site := r.client.SiteOrDefault(state.Site)
	dev, switchable, diags := r.apply(ctx, site, &plan, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.deviceToModel(dev, &plan, site, switchable)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete releases every port this resource manages: their overrides are
// removed, so they return to the controller's defaults (which, for a UniFi
// switch, is the Default network with every VLAN tagged).
func (r *devicePortOverridesResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state devicePortOverridesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	site := r.client.SiteOrDefault(state.Site)
	empty := devicePortOverridesModel{MAC: state.MAC, Ports: map[string]portOverrideModel{}, DefaultPortProfileID: types.StringNull()}
	_, _, diags := r.apply(ctx, site, &empty, &state)
	for _, d := range diags {
		if d.Summary() == "Device Not Found" {
			return
		}
	}
	resp.Diagnostics.Append(diags...)
}

// ImportState handles `terraform import terrifi_device_port_overrides.name <mac>`
// or `<site>:<mac>`. Every port that has a profile assigned is imported.
func (r *devicePortOverridesResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	id := req.ID
	// A MAC contains colons, so the site prefix is whatever precedes the last 17 characters.
	if len(id) > 17 && id[len(id)-18] == ':' {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("site"), id[:len(id)-18])...)
		id = id[len(id)-17:]
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("mac"), strings.ToLower(id))...)
}

// ValidateConfig checks each port is either on a profile or fully configured
// on the port itself, but not both.
func (r *devicePortOverridesResource) ValidateConfig(
	ctx context.Context,
	req resource.ValidateConfigRequest,
	resp *resource.ValidateConfigResponse,
) {
	var m devicePortOverridesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for key, po := range m.Ports {
		resp.Diagnostics.Append(validatePortOverride(path.Root("ports").AtMapKey(key), po)...)
	}
}

func validatePortOverride(base path.Path, po portOverrideModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if !po.PortProfileID.IsNull() {
		inline := map[string]attr.Value{
			"native_network_id":  po.NativeNetworkID,
			"tagged_vlan_mgmt":   po.TaggedVLANMgmt,
			"tagged_network_ids": po.TaggedNetworkIDs,
			"poe_mode":           po.PoeMode,
			"stp_edge_state":     po.StpEdgeState,
		}
		for name, v := range inline {
			if !v.IsNull() {
				diags.AddAttributeError(base.AtName(name), "Conflicting port settings",
					name+" cannot be set together with port_profile_id; the profile decides it.")
			}
		}
		return diags
	}
	if po.TaggedVLANMgmt.IsNull() {
		diags.AddAttributeError(base.AtName("tagged_vlan_mgmt"), "Missing port settings",
			"Set either port_profile_id or tagged_vlan_mgmt (with native_network_id as needed).")
		return diags
	}
	return validateTaggingAt(base, po.TaggedVLANMgmt, po.TaggedNetworkIDs)
}

// ---------------------------------------------------------------------------
// Helper methods
// ---------------------------------------------------------------------------

// apply computes the device's new port_overrides from the plan (and, on
// update/delete, the prior state), writes them, and waits for the switch to
// reconnect. It returns the device as read after the write, plus the site's
// switchable networks used to resolve tagged VLANs.
func (r *devicePortOverridesResource) apply(
	ctx context.Context,
	site string,
	plan *devicePortOverridesModel,
	prior *devicePortOverridesModel,
) (*SwitchDevice, []string, diag.Diagnostics) {
	var diags diag.Diagnostics
	mac := plan.MAC.ValueString()

	dev, err := r.client.GetSwitchDevice(ctx, site, mac)
	if err != nil {
		if _, ok := err.(*unifi.NotFoundError); ok {
			diags.AddError("Device Not Found", fmt.Sprintf("No device with MAC %s on site %s.", mac, site))
			return nil, nil, diags
		}
		diags.AddError("Error Reading Device", err.Error())
		return nil, nil, diags
	}
	if len(dev.PortTable) == 0 {
		diags.AddError("Device Has No Switch Ports",
			fmt.Sprintf("Device %s (%s) reports no port table; is it an adopted switch?", mac, dev.Model))
		return nil, nil, diags
	}

	switchable, err := r.client.SwitchableNetworkIDs(ctx, site)
	if err != nil {
		diags.AddError("Error Listing Networks", err.Error())
		return nil, nil, diags
	}

	overrides, d := buildPortOverrides(ctx, dev, plan, prior, switchable)
	diags.Append(d...)
	if diags.HasError() {
		return nil, nil, diags
	}

	if err := r.client.SetPortOverrides(ctx, site, dev.ID, overrides); err != nil {
		diags.AddError("Error Updating Port Overrides", err.Error())
		return nil, nil, diags
	}

	dev, err = r.client.WaitForDeviceConnected(ctx, site, mac, devicePortProvisionTimeout)
	if err != nil {
		diags.AddError("Error Waiting For Device", err.Error())
		return nil, nil, diags
	}
	return dev, switchable, diags
}

// buildPortOverrides is the pure part of apply: given the device as read and
// the desired/prior models, it returns the full port_overrides array to send.
//
//   - Ports on a profile get exactly {port_idx, name, portconf_id}, replacing
//     any settings made on the port in the UI.
//   - Ports configured inline keep their existing override (speed, advanced
//     settings, ...) with only the managed keys rewritten.
//   - With a default profile, every other port except the uplink gets it.
//   - Ports this resource managed before but no longer does lose their
//     override (back to controller defaults), keeping only their name.
//   - Everything else is passed through untouched, as raw JSON.
func buildPortOverrides(
	ctx context.Context,
	dev *SwitchDevice,
	plan *devicePortOverridesModel,
	prior *devicePortOverridesModel,
	switchable []string,
) ([]map[string]json.RawMessage, diag.Diagnostics) {
	var diags diag.Diagnostics

	ports := make(map[int64]SwitchPort, len(dev.PortTable))
	for _, p := range dev.PortTable {
		ports[p.PortIdx] = p
	}

	listed := make(map[int64]portOverrideModel, len(plan.Ports))
	for key, po := range plan.Ports {
		idx, _ := strconv.ParseInt(key, 10, 64)
		p, ok := ports[idx]
		if !ok {
			diags.AddAttributeError(path.Root("ports").AtMapKey(key), "Unknown port",
				fmt.Sprintf("Device %s has no port %d (it has %d ports).", dev.MAC, idx, len(dev.PortTable)))
			continue
		}
		if p.IsUplink {
			diags.AddAttributeWarning(path.Root("ports").AtMapKey(key), "Port is the uplink",
				fmt.Sprintf("Port %d is this switch's current uplink. Settings that do not carry the "+
					"management network on it will disconnect the switch.", idx))
		}
		listed[idx] = po
	}
	if diags.HasError() {
		return nil, diags
	}

	defaultProfile := plan.DefaultPortProfileID.ValueString()

	// Ports the prior state managed: its listed ports, and ports carrying its
	// default profile.
	priorListed := map[int64]bool{}
	priorDefault := ""
	if prior != nil {
		for key := range prior.Ports {
			idx, _ := strconv.ParseInt(key, 10, 64)
			priorListed[idx] = true
		}
		priorDefault = prior.DefaultPortProfileID.ValueString()
	}

	existing := make(map[int64]map[string]json.RawMessage, len(dev.PortOverrides))
	var orphans []map[string]json.RawMessage // overrides with no port_idx, kept as-is
	for _, o := range dev.PortOverrides {
		if idx := overridePortIdx(o); idx >= 0 {
			existing[idx] = o
		} else {
			orphans = append(orphans, o)
		}
	}

	// Every port index that has or may get an override, in order.
	idxSet := map[int64]bool{}
	for idx := range ports {
		idxSet[idx] = true
	}
	for idx := range existing {
		idxSet[idx] = true
	}
	indexes := make([]int64, 0, len(idxSet))
	for idx := range idxSet {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })

	out := make([]map[string]json.RawMessage, 0, len(indexes)+len(orphans))
	for _, idx := range indexes {
		old, hasOld := existing[idx]
		port, isPort := ports[idx]

		switch po, isListed := listed[idx]; {
		case isListed && po.isInline():
			base := path.Root("ports").AtMapKey(strconv.FormatInt(idx, 10))
			o, d := inlineOverride(ctx, base, idx, old, po, switchable)
			diags.Append(d...)
			out = append(out, o)

		case isListed:
			name := po.Name.ValueString()
			if po.Name.IsNull() && hasOld {
				name = overrideString(old, "name")
			}
			out = append(out, newOverride(idx, name, po.PortProfileID.ValueString()))

		case defaultProfile != "" && isPort && !port.IsUplink:
			out = append(out, newOverride(idx, overrideString(old, "name"), defaultProfile))

		case hasOld && (priorListed[idx] ||
			(priorDefault != "" && overrideString(old, "portconf_id") == priorDefault)):
			// Released: keep only the name, if the port had one.
			if name := overrideString(old, "name"); name != "" {
				out = append(out, newOverride(idx, name, ""))
			}

		case hasOld:
			out = append(out, old)
		}
	}
	if diags.HasError() {
		return nil, diags
	}
	return append(out, orphans...), diags
}

// inlineOverride builds the override for a port configured on the port
// itself. It starts from the port's existing inline override so settings this
// resource does not manage (link speed, advanced options) are kept; an
// override that pointed at a profile is started fresh instead.
func inlineOverride(
	ctx context.Context,
	base path.Path,
	idx int64,
	old map[string]json.RawMessage,
	po portOverrideModel,
	switchable []string,
) (map[string]json.RawMessage, diag.Diagnostics) {
	var diags diag.Diagnostics

	o := map[string]json.RawMessage{}
	if overrideString(old, "portconf_id") == "" {
		for k, v := range old {
			o[k] = v
		}
	}
	delete(o, "portconf_id")
	o["port_idx"] = mustJSON(idx)

	name := po.Name.ValueString()
	if po.Name.IsNull() {
		name = overrideString(old, "name")
	}
	if name != "" {
		o["name"] = mustJSON(name)
	} else {
		delete(o, "name")
	}

	isSwitchable := make(map[string]bool, len(switchable))
	for _, id := range switchable {
		isSwitchable[id] = true
	}

	native := po.NativeNetworkID.ValueString() // null → "" → Native VLAN: None
	if native != "" && !isSwitchable[native] {
		diags.AddAttributeError(base.AtName("native_network_id"), "Unknown native network",
			fmt.Sprintf("Network %q is not a LAN network on this site.", native))
	}
	o["native_networkconf_id"] = mustJSON(native)

	mode := po.TaggedVLANMgmt.ValueString()
	o["tagged_vlan_mgmt"] = mustJSON(mode)
	excluded := []string{}
	if mode == taggedVLANCustom {
		var tagged []string
		diags.Append(po.TaggedNetworkIDs.ElementsAs(ctx, &tagged, false)...)
		for _, id := range tagged {
			switch {
			case id == native:
				diags.AddAttributeError(base.AtName("tagged_network_ids"), "Native network listed as tagged",
					fmt.Sprintf("Network %q is the native network; a port cannot also carry it tagged.", id))
			case !isSwitchable[id]:
				diags.AddAttributeError(base.AtName("tagged_network_ids"), "Unknown tagged network",
					fmt.Sprintf("Network %q is not a LAN network on this site.", id))
			}
		}
		excluded = excludedFromTagged(switchable, tagged, native)
	}
	o["excluded_networkconf_ids"] = mustJSON(excluded)
	o["forward"] = mustJSON(forwardFor(mode, native))

	if !po.PoeMode.IsNull() {
		o["poe_mode"] = mustJSON(po.PoeMode.ValueString())
	}
	if !po.StpEdgeState.IsNull() {
		o["stp_edge_state"] = mustJSON(po.StpEdgeState.ValueString())
	}
	return o, diags
}

// forwardFor is the controller's legacy forward value for a tagging mode,
// kept consistent so older readers of the override agree with it.
func forwardFor(mode, native string) string {
	switch mode {
	case taggedVLANAllowAll:
		return "all"
	case taggedVLANCustom:
		return "customize"
	default:
		if native == "" {
			return "disabled"
		}
		return "native"
	}
}

// taggedModeOf reads an override's tagging mode, falling back to its forward
// value for overrides written before tagged_vlan_mgmt existed.
func taggedModeOf(o map[string]json.RawMessage) string {
	if mode := overrideString(o, "tagged_vlan_mgmt"); mode != "" {
		return mode
	}
	switch overrideString(o, "forward") {
	case "customize":
		return taggedVLANCustom
	case "native", "disabled":
		return taggedVLANBlockAll
	default:
		return taggedVLANAllowAll
	}
}

func overrideStrings(o map[string]json.RawMessage, key string) []string {
	var s []string
	if raw, ok := o[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func newOverride(idx int64, name, profileID string) map[string]json.RawMessage {
	o := map[string]json.RawMessage{"port_idx": mustJSON(idx)}
	if name != "" {
		o["name"] = mustJSON(name)
	}
	if profileID != "" {
		o["portconf_id"] = mustJSON(profileID)
	}
	return o
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func optionalString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// readPortOverride fills po from the port's override. all is true on import,
// when every attribute the override carries is taken in; otherwise optional
// keep-current attributes (name, poe_mode, stp_edge_state) are only refreshed
// if the configuration sets them.
func readPortOverride(po *portOverrideModel, o map[string]json.RawMessage, switchable []string, all bool) {
	if all || !po.Name.IsNull() {
		po.Name = optionalString(overrideString(o, "name"))
	}

	if id := overrideString(o, "portconf_id"); id != "" {
		po.PortProfileID = types.StringValue(id)
		// Profile ports carry no per-port settings of their own.
		if all {
			po.NativeNetworkID = types.StringNull()
			po.TaggedVLANMgmt = types.StringNull()
			po.TaggedNetworkIDs = types.SetNull(types.StringType)
			po.PoeMode = types.StringNull()
			po.StpEdgeState = types.StringNull()
		}
		return
	}

	po.PortProfileID = types.StringNull()
	if !all && po.TaggedVLANMgmt.IsNull() {
		// Configured for a profile, but the port has none: the null profile
		// ID above already plans the fix.
		return
	}

	native := overrideString(o, "native_networkconf_id")
	po.NativeNetworkID = optionalString(native)
	mode := taggedModeOf(o)
	po.TaggedVLANMgmt = types.StringValue(mode)
	if mode == taggedVLANCustom {
		po.TaggedNetworkIDs = stringSetValue(TaggedFromExcluded(switchable, overrideStrings(o, "excluded_networkconf_ids"), native))
	} else {
		po.TaggedNetworkIDs = types.SetNull(types.StringType)
	}
	if all || !po.PoeMode.IsNull() {
		po.PoeMode = optionalString(overrideString(o, "poe_mode"))
	}
	if all || !po.StpEdgeState.IsNull() {
		po.StpEdgeState = optionalString(overrideString(o, "stp_edge_state"))
	}
}

// deviceToModel refreshes the model from the device. Ports keep the keys the
// model already has; on import (no ports yet) every port with an override is
// taken in, whether it uses a profile or was configured by hand in the UI.
func (r *devicePortOverridesResource) deviceToModel(
	dev *SwitchDevice,
	m *devicePortOverridesModel,
	site string,
	switchable []string,
) {
	m.ID = types.StringValue(dev.ID)
	m.Site = types.StringValue(site)
	m.MAC = types.StringValue(strings.ToLower(dev.MAC))

	existing := make(map[int64]map[string]json.RawMessage, len(dev.PortOverrides))
	for _, o := range dev.PortOverrides {
		if idx := overridePortIdx(o); idx >= 0 {
			existing[idx] = o
		}
	}

	if m.Ports == nil {
		m.Ports = map[string]portOverrideModel{}
		for idx, o := range existing {
			var po portOverrideModel
			readPortOverride(&po, o, switchable, true)
			m.Ports[strconv.FormatInt(idx, 10)] = po
		}
		m.DefaultPortProfileID = types.StringNull()
		return
	}

	listed := map[int64]bool{}
	for key, po := range m.Ports {
		idx, _ := strconv.ParseInt(key, 10, 64)
		listed[idx] = true
		readPortOverride(&po, existing[idx], switchable, false)
		m.Ports[key] = po
	}

	if def := m.DefaultPortProfileID.ValueString(); def != "" {
		for _, p := range dev.PortTable {
			if listed[p.PortIdx] || p.IsUplink {
				continue
			}
			if overrideString(existing[p.PortIdx], "portconf_id") != def {
				m.DefaultPortProfileID = types.StringNull()
				break
			}
		}
	}
}
