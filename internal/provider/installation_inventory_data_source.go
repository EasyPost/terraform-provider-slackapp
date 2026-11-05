package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ymm-oss/terraform-provider-slackapp/internal/slack"
)

type installationInventoryDataSourceModel struct {
	WorkspaceID              types.String `tfsdk:"workspace_id"`
	AppIDs                   types.Set    `tfsdk:"app_ids"`
	Statuses                 types.Map    `tfsdk:"statuses"`
	ObservedAt               types.String `tfsdk:"observed_at"`
	AllRequestedAppsObserved types.Bool   `tfsdk:"all_requested_apps_observed"`
}

type installationInventoryDataSource struct {
	client *slack.Client
}

//declscope:shared // provider.go registers it
func newInstallationInventoryDataSource() datasource.DataSource {
	return &installationInventoryDataSource{}
}

func (d *installationInventoryDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, response *datasource.MetadataResponse) {
	response.TypeName = "slackapp_installation_inventory"
}

func (d *installationInventoryDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, response *datasource.SchemaResponse) {
	response.Schema = schema.Schema{
		MarkdownDescription: "Experimental read-only installation observations for supplied app IDs in one workspace, " +
			"using the Slack CLI's apps.status API. Include known owned or third-party IDs from a reviewed catalog; " +
			"visibility depends on the service token and remains unverified for third-party apps. " +
			"An omitted ID or flag is unknown, never uninstalled. " +
			"This does not discover IDs or attest a complete workspace inventory or granted token permissions.",
		Attributes: map[string]schema.Attribute{
			"workspace_id": schema.StringAttribute{
				Required:   true,
				Validators: []validator.String{stringvalidator.RegexMatches(installationResourceWorkspaceID, "Workspace IDs must start with T and contain uppercase letters or digits.")},
			},
			"app_ids": schema.SetAttribute{
				Required: true, ElementType: types.StringType,
				Validators: []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(
					stringvalidator.RegexMatches(installationResourceAppID, "App IDs must start with A and contain uppercase letters or digits."))},
			},
			"statuses": schema.MapAttribute{
				MarkdownDescription: "Every requested app ID mapped to installed, uninstalled, or unknown.",
				Computed:            true, ElementType: types.StringType,
			},
			"observed_at": schema.StringAttribute{
				MarkdownDescription: "UTC timestamp when this response was observed.",
				Computed:            true,
			},
			"all_requested_apps_observed": schema.BoolAttribute{
				MarkdownDescription: "Whether Slack supplied an explicit status for every requested ID. This does not establish full workspace coverage.",
				Computed:            true,
			},
		},
	}
}

func (d *installationInventoryDataSource) Configure(_ context.Context, request datasource.ConfigureRequest, response *datasource.ConfigureResponse) {
	if request.ProviderData == nil {
		return
	}
	client, ok := request.ProviderData.(*slack.Client)
	if !ok {
		response.Diagnostics.AddError("Invalid provider configuration", "Expected a Slack client.")
		return
	}
	d.client = client
}

func (d *installationInventoryDataSource) Read(ctx context.Context, request datasource.ReadRequest, response *datasource.ReadResponse) {
	var data installationInventoryDataSourceModel
	response.Diagnostics.Append(request.Config.Get(ctx, &data)...)
	if response.Diagnostics.HasError() {
		return
	}
	var appIDs []string
	response.Diagnostics.Append(data.AppIDs.ElementsAs(ctx, &appIDs, false)...)
	if response.Diagnostics.HasError() {
		return
	}
	statuses, err := d.client.DeveloperAppStatus(ctx, data.WorkspaceID.ValueString(), appIDs)
	if err != nil {
		response.Diagnostics.AddError("Unable to observe installation inventory", err.Error())
		return
	}
	values := make(map[string]attr.Value, len(statuses))
	allObserved := true
	for appID, status := range statuses {
		values[appID] = types.StringValue(string(status))
		allObserved = allObserved && status != slack.InstallationUnknown
	}
	data.Statuses = types.MapValueMust(types.StringType, values)
	data.AllRequestedAppsObserved = types.BoolValue(allObserved)
	data.ObservedAt = types.StringValue(time.Now().UTC().Format(time.RFC3339))
	response.Diagnostics.Append(response.State.Set(ctx, &data)...)
}
