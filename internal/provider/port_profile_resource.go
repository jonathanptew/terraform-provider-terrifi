package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/ubiquiti-community/go-unifi/unifi"
)

// Compile-time interface checks.
var (
	_ resource.Resource                   = &portProfileResource{}
	_ resource.ResourceWithImportState    = &portProfileResource{}
	_ resource.ResourceWithValidateConfig = &portProfileResource{}
)

// Tagged VLAN management modes, as the controller names them.
const (
	taggedVLANAllowAll = "auto"
	taggedVLANBlockAll = "block_all"
	taggedVLANCustom   = "custom"
)

// NewPortProfileResource is the factory function registered in provider.Resources().
func NewPortProfileResource() resource.Resource {
	return &portProfileResource{}
}

// portProfileResource holds the API client, injected by Configure().
type portProfileResource struct {
	client *Client
}

// portProfileResourceModel is the Terraform-side representation of a switch
// port profile.
type portProfileResourceModel struct {
	ID                       types.String `tfsdk:"id"`
	Site                     types.String `tfsdk:"site"`
	Name                     types.String `tfsdk:"name"`
	NativeNetworkID          types.String `tfsdk:"native_network_id"`
	TaggedVLANMgmt           types.String `tfsdk:"tagged_vlan_mgmt"`
	TaggedNetworkIDs         types.Set    `tfsdk:"tagged_network_ids"`
	PoeMode                  types.String `tfsdk:"poe_mode"`
	Isolation                types.Bool   `tfsdk:"isolation"`
	PortSecurityMACAddresses types.Set    `tfsdk:"port_security_mac_addresses"`
	StpEdgeState             types.String `tfsdk:"stp_edge_state"`
	Forward                  types.String `tfsdk:"forward"`
}

// Metadata sets the resource type name.
func (r *portProfileResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_port_profile"
}

// Schema defines the HCL schema for the terrifi_port_profile resource.
func (r *portProfileResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a switch port profile (Devices > Port Manager > Port Profiles) on the " +
			"UniFi controller. A profile sets a port's native (untagged) network, which tagged networks it " +
			"carries, PoE and port security. Unset settings default to a locked-down port: no native network, " +
			"no tagged networks.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The ID of the port profile.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			"site": schema.StringAttribute{
				MarkdownDescription: "The site to associate the port profile with. Defaults to the provider site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			"name": schema.StringAttribute{
				MarkdownDescription: "The name of the port profile.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 128),
				},
			},

			"native_network_id": schema.StringAttribute{
				MarkdownDescription: "The ID of the network carried untagged on the port. Leave unset for " +
					"**Native VLAN: None** (no untagged traffic). Note that the controller itself treats a missing " +
					"native network as the Default network; this resource always sends None explicitly.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},

			"tagged_vlan_mgmt": schema.StringAttribute{
				MarkdownDescription: "Which tagged networks the port carries. One of `block_all` (none, an access " +
					"port), `custom` (only `tagged_network_ids`), `auto` (every network, the controller's " +
					"\"Allow All\"). Defaults to `block_all`.",
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(taggedVLANBlockAll),
				Validators: []validator.String{
					stringvalidator.OneOf(taggedVLANBlockAll, taggedVLANCustom, taggedVLANAllowAll),
				},
			},

			"tagged_network_ids": schema.SetAttribute{
				MarkdownDescription: "The networks carried tagged when `tagged_vlan_mgmt = \"custom\"`. Required " +
					"(and non-empty) for `custom`, not allowed otherwise. The controller stores this as an " +
					"exclusion list and adds every newly created network to it, so new networks are not " +
					"carried until they are listed here.",
				Optional:    true,
				ElementType: types.StringType,
			},

			"poe_mode": schema.StringAttribute{
				MarkdownDescription: "PoE output. One of `auto`, `off`. Defaults to `auto`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("auto"),
				Validators: []validator.String{
					stringvalidator.OneOf("auto", "off"),
				},
			},

			"isolation": schema.BoolAttribute{
				MarkdownDescription: "Port isolation: ports with isolation on cannot talk to each other on the " +
					"switch. Defaults to `false`.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
			},

			"port_security_mac_addresses": schema.SetAttribute{
				MarkdownDescription: "MAC addresses allowed on the port (MAC ID restriction), lower case. " +
					"Setting any enables port security; leave unset to turn it off.",
				Optional:    true,
				ElementType: types.StringType,
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(stringvalidator.RegexMatches(
						regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`),
						"must be a lower-case MAC address, e.g. aa:bb:cc:dd:ee:ff",
					)),
				},
			},

			"stp_edge_state": schema.StringAttribute{
				MarkdownDescription: "Port Mode in the UI: `enabled` is **Edge** (an end device; the port " +
					"forwards immediately and is never an STP path to the root), `disabled` is " +
					"**Infrastructure** (another switch or AP). Leave unset to use the controller's default; " +
					"profiles cannot store `auto`, and removing the attribute keeps the stored value.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
				Validators: []validator.String{
					stringvalidator.OneOf("enabled", "disabled"),
				},
			},

			"forward": schema.StringAttribute{
				MarkdownDescription: "The forwarding mode the controller derived from the settings above " +
					"(`all`, `native`, `customize`, `disabled`). Read-only.",
				Computed: true,
			},
		},
	}
}

// ValidateConfig checks the tagged_vlan_mgmt / tagged_network_ids pairing.
// The controller rewrites `custom` with nothing tagged to `block_all`, so an
// empty custom list would never converge.
func (r *portProfileResource) ValidateConfig(
	ctx context.Context,
	req resource.ValidateConfigRequest,
	resp *resource.ValidateConfigResponse,
) {
	var cfg portProfileResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validatePortProfileTagging(cfg.TaggedVLANMgmt, cfg.TaggedNetworkIDs)...)
}

func validatePortProfileTagging(mode types.String, tagged types.Set) diag.Diagnostics {
	return validateTaggingAt(path.Empty(), mode, tagged)
}

// validateTaggingAt checks tagged_vlan_mgmt / tagged_network_ids under base,
// so per-port settings can share the profile's rules.
func validateTaggingAt(base path.Path, mode types.String, tagged types.Set) diag.Diagnostics {
	var diags diag.Diagnostics
	if mode.IsUnknown() || tagged.IsUnknown() {
		return diags
	}
	isCustom := mode.ValueString() == taggedVLANCustom
	hasTagged := !tagged.IsNull() && len(tagged.Elements()) > 0

	switch {
	case isCustom && tagged.IsNull():
		diags.AddAttributeError(base.AtName("tagged_network_ids"), "Missing tagged networks",
			"tagged_network_ids is required when tagged_vlan_mgmt is \"custom\".")
	case isCustom && !hasTagged:
		diags.AddAttributeError(base.AtName("tagged_network_ids"), "Empty tagged networks",
			"tagged_network_ids must not be empty when tagged_vlan_mgmt is \"custom\"; "+
				"use tagged_vlan_mgmt = \"block_all\" for a port with no tagged networks.")
	case !isCustom && !tagged.IsNull():
		diags.AddAttributeError(base.AtName("tagged_network_ids"), "Unexpected tagged networks",
			"tagged_network_ids can only be set when tagged_vlan_mgmt is \"custom\".")
	}
	return diags
}

// Configure is called by the framework to inject the provider's API client.
func (r *portProfileResource) Configure(
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

// Create creates a new port profile.
func (r *portProfileResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var plan portProfileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	site := r.client.SiteOrDefault(plan.Site)
	created, switchable, diags := r.write(ctx, site, &plan, "")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.apiToModel(created, &plan, site, switchable)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the Terraform state from the actual API state.
func (r *portProfileResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state portProfileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	site := r.client.SiteOrDefault(state.Site)

	profile, err := r.client.GetPortProfile(ctx, site, state.ID.ValueString())
	if err != nil {
		if _, ok := err.(*unifi.NotFoundError); ok {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Error Reading Port Profile",
			fmt.Sprintf("Could not read port profile %s: %s", state.ID.ValueString(), err.Error()),
		)
		return
	}

	switchable, err := r.client.SwitchableNetworkIDs(ctx, site)
	if err != nil {
		resp.Diagnostics.AddError("Error Listing Networks", err.Error())
		return
	}

	r.apiToModel(profile, &state, site, switchable)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update updates an existing port profile. Every managed attribute is either
// required, defaulted, or null-means-off, so the plan is the complete desired
// object and is sent as-is.
func (r *portProfileResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var state, plan portProfileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	site := r.client.SiteOrDefault(state.Site)
	updated, switchable, diags := r.write(ctx, site, &plan, state.ID.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	plan.ID = state.ID
	r.apiToModel(updated, &plan, site, switchable)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the port profile from the UniFi controller.
func (r *portProfileResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state portProfileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	site := r.client.SiteOrDefault(state.Site)

	err := r.client.DeletePortProfile(ctx, site, state.ID.ValueString())
	if err != nil {
		if _, ok := err.(*unifi.NotFoundError); ok {
			return
		}
		resp.Diagnostics.AddError("Error Deleting Port Profile", err.Error())
	}
}

// ImportState handles `terraform import terrifi_port_profile.name <id>`.
// Supports both "id" and "site:id" formats.
func (r *portProfileResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	parts := strings.SplitN(req.ID, ":", 2)

	if len(parts) == 2 {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("site"), parts[0])...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), parts[1])...)
		return
	}

	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// ---------------------------------------------------------------------------
// Helper methods
// ---------------------------------------------------------------------------

// write creates (id == "") or updates the profile and returns what the
// controller stored, along with the switchable network list it was resolved
// against.
//
// The controller adds every newly created network to the exclusion list of
// each existing custom profile, so new networks are never carried by
// accident. A network created while this profile is being written (e.g. by
// another resource in the same apply) can slip past both that and our own
// listing, and would end up tagged. So for custom profiles the stored object
// is checked against the plan once the write is done, and re-sent if a
// network appeared in between.
func (r *portProfileResource) write(
	ctx context.Context,
	site string,
	plan *portProfileResourceModel,
	id string,
) (*PortProfile, []string, diag.Diagnostics) {
	var diags diag.Diagnostics

	switchable, err := r.client.SwitchableNetworkIDs(ctx, site)
	if err != nil {
		diags.AddError("Error Listing Networks", err.Error())
		return nil, nil, diags
	}

	var want []string
	if plan.TaggedVLANMgmt.ValueString() == taggedVLANCustom {
		diags.Append(plan.TaggedNetworkIDs.ElementsAs(ctx, &want, false)...)
		if diags.HasError() {
			return nil, nil, diags
		}
	}

	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		payload, d := r.modelToAPI(ctx, plan, switchable)
		diags.Append(d...)
		if diags.HasError() {
			return nil, nil, diags
		}

		var stored *PortProfile
		if id == "" {
			stored, err = r.client.CreatePortProfile(ctx, site, payload)
			if err != nil {
				diags.AddError("Error Creating Port Profile", err.Error())
				return nil, nil, diags
			}
			id = stored.ID
		} else {
			payload.ID = id
			stored, err = r.client.UpdatePortProfile(ctx, site, payload)
			if err != nil {
				diags.AddError("Error Updating Port Profile", err.Error())
				return nil, nil, diags
			}
		}
		if want == nil {
			return stored, switchable, diags
		}

		// Give concurrent network writes a moment to land, then check.
		if err := sleepCtx(ctx, time.Duration(attempt)*500*time.Millisecond); err != nil {
			diags.AddError("Error Updating Port Profile", err.Error())
			return nil, nil, diags
		}
		if switchable, err = r.client.SwitchableNetworkIDs(ctx, site); err != nil {
			diags.AddError("Error Listing Networks", err.Error())
			return nil, nil, diags
		}
		if stored, err = r.client.GetPortProfile(ctx, site, id); err != nil {
			diags.AddError("Error Reading Port Profile", err.Error())
			return nil, nil, diags
		}
		got := TaggedFromExcluded(switchable, stored.ExcludedNetworkIDs, stored.NativeNetworkID)
		if sameStrings(got, want) || attempt == maxAttempts {
			return stored, switchable, diags
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			return false
		}
	}
	return true
}

// modelToAPI converts the Terraform model to the rest/portconf payload.
// switchable is the site's taggable network IDs (see SwitchableNetworkIDs),
// used to turn tagged_network_ids into the controller's exclusion list.
func (r *portProfileResource) modelToAPI(
	ctx context.Context,
	m *portProfileResourceModel,
	switchable []string,
) (*PortProfile, diag.Diagnostics) {
	var diags diag.Diagnostics

	p := &PortProfile{
		Name:            m.Name.ValueString(),
		NativeNetworkID: m.NativeNetworkID.ValueString(), // null → "" → Native VLAN: None
		TaggedVLANMgmt:  m.TaggedVLANMgmt.ValueString(),
		PoeMode:         m.PoeMode.ValueString(),
		OpMode:          "switch",
		Isolation:       m.Isolation.ValueBool(),
		StpEdgeState:    m.StpEdgeState.ValueString(), // unknown/null → "" → omitted
	}
	if p.TaggedVLANMgmt == "" {
		p.TaggedVLANMgmt = taggedVLANBlockAll
	}

	// The controller silently replaces an unknown native network with Default
	// and drops unknown tagged IDs, so catch both here instead of reporting an
	// inconsistent result after apply.
	isSwitchable := make(map[string]bool, len(switchable))
	for _, id := range switchable {
		isSwitchable[id] = true
	}
	if p.NativeNetworkID != "" && !isSwitchable[p.NativeNetworkID] {
		diags.AddAttributeError(path.Root("native_network_id"), "Unknown native network",
			fmt.Sprintf("Network %q is not a LAN network on this site.", p.NativeNetworkID))
	}

	if p.TaggedVLANMgmt == taggedVLANCustom {
		var tagged []string
		diags.Append(m.TaggedNetworkIDs.ElementsAs(ctx, &tagged, false)...)
		if diags.HasError() {
			return nil, diags
		}
		for _, id := range tagged {
			switch {
			case id == p.NativeNetworkID:
				diags.AddAttributeError(path.Root("tagged_network_ids"), "Native network listed as tagged",
					fmt.Sprintf("Network %q is the native network; a port cannot also carry it tagged.", id))
			case !isSwitchable[id]:
				diags.AddAttributeError(path.Root("tagged_network_ids"), "Unknown tagged network",
					fmt.Sprintf("Network %q is not a LAN network on this site.", id))
			}
		}
		p.ExcludedNetworkIDs = excludedFromTagged(switchable, tagged, p.NativeNetworkID)
	}
	if diags.HasError() {
		return nil, diags
	}

	if !m.PortSecurityMACAddresses.IsNull() && !m.PortSecurityMACAddresses.IsUnknown() {
		var macs []string
		diags.Append(m.PortSecurityMACAddresses.ElementsAs(ctx, &macs, false)...)
		if diags.HasError() {
			return nil, diags
		}
		sort.Strings(macs)
		p.PortSecurityMACAddress = macs
		p.PortSecurityEnabled = len(macs) > 0
	}

	return p, diags
}

// apiToModel converts the stored rest/portconf object back to the model.
func (r *portProfileResource) apiToModel(
	p *PortProfile,
	m *portProfileResourceModel,
	site string,
	switchable []string,
) {
	m.ID = types.StringValue(p.ID)
	m.Site = types.StringValue(site)
	m.Name = types.StringValue(p.Name)

	if p.NativeNetworkID == "" {
		m.NativeNetworkID = types.StringNull()
	} else {
		m.NativeNetworkID = types.StringValue(p.NativeNetworkID)
	}

	// Profiles made in the UI before tagged_vlan_mgmt existed omit it; the
	// controller treats that as Allow All.
	mode := p.TaggedVLANMgmt
	if mode == "" {
		mode = taggedVLANAllowAll
	}
	m.TaggedVLANMgmt = types.StringValue(mode)

	if mode == taggedVLANCustom {
		m.TaggedNetworkIDs = stringSetValue(TaggedFromExcluded(switchable, p.ExcludedNetworkIDs, p.NativeNetworkID))
	} else {
		m.TaggedNetworkIDs = types.SetNull(types.StringType)
	}

	poe := p.PoeMode
	if poe == "" {
		poe = "auto"
	}
	m.PoeMode = types.StringValue(poe)
	m.Isolation = types.BoolValue(p.Isolation)

	if p.PortSecurityEnabled && len(p.PortSecurityMACAddress) > 0 {
		macs := make([]string, len(p.PortSecurityMACAddress))
		for i, mac := range p.PortSecurityMACAddress {
			macs[i] = strings.ToLower(mac)
		}
		m.PortSecurityMACAddresses = stringSetValue(macs)
	} else {
		m.PortSecurityMACAddresses = types.SetNull(types.StringType)
	}

	if p.StpEdgeState == "" {
		m.StpEdgeState = types.StringNull()
	} else {
		m.StpEdgeState = types.StringValue(p.StpEdgeState)
	}

	m.Forward = types.StringValue(p.Forward)
}

// excludedFromTagged returns the switchable networks that are neither tagged
// nor the native network, which is the exclusion list the controller stores.
func excludedFromTagged(switchable, tagged []string, native string) []string {
	keep := make(map[string]bool, len(tagged)+1)
	for _, id := range tagged {
		keep[id] = true
	}
	if native != "" {
		keep[native] = true
	}
	excluded := make([]string, 0, len(switchable))
	for _, id := range switchable {
		if !keep[id] {
			excluded = append(excluded, id)
		}
	}
	sort.Strings(excluded)
	return excluded
}

// TaggedFromExcluded is the inverse of excludedFromTagged: the switchable
// networks that are not excluded and not the native network.
func TaggedFromExcluded(switchable, excluded []string, native string) []string {
	drop := make(map[string]bool, len(excluded)+1)
	for _, id := range excluded {
		drop[id] = true
	}
	if native != "" {
		drop[native] = true
	}
	tagged := make([]string, 0, len(switchable))
	for _, id := range switchable {
		if !drop[id] {
			tagged = append(tagged, id)
		}
	}
	sort.Strings(tagged)
	return tagged
}

func stringSetValue(vals []string) types.Set {
	elems := make([]attr.Value, len(vals))
	for i, v := range vals {
		elems[i] = types.StringValue(v)
	}
	return types.SetValueMust(types.StringType, elems)
}
