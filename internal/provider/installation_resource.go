package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ymm-oss/terraform-provider-slackapp/internal/slack"
)

//declscope:shared // the inventory validates the same identities
var installationResourceAppID = regexp.MustCompile(`^A[A-Z0-9]+$`)

//declscope:shared // the inventory validates the same identities
var installationResourceWorkspaceID = regexp.MustCompile(`^T[A-Z0-9]+$`)

type installationResourceModel struct {
	ID              types.String `tfsdk:"id"`
	AppID           types.String `tfsdk:"app_id"`
	WorkspaceID     types.String `tfsdk:"workspace_id"`
	BotScopes       types.Set    `tfsdk:"bot_scopes"`
	OutgoingDomains types.Set    `tfsdk:"outgoing_domains"`
	Installed       types.Bool   `tfsdk:"installed"`
	BotToken        types.String `tfsdk:"bot_token"`
	AppLevelToken   types.String `tfsdk:"app_level_token"`
	UserToken       types.String `tfsdk:"user_token"`
}

type installationResource struct {
	client *slack.Client
}

//declscope:shared // provider.go registers it
func newInstallationResource() resource.Resource {
	return &installationResource{}
}

func (r *installationResource) Metadata(_ context.Context, _ resource.MetadataRequest, response *resource.MetadataResponse) {
	response.TypeName = "slackapp_installation"
}

func (r *installationResource) Schema(_ context.Context, _ resource.SchemaRequest, response *resource.SchemaResponse) {
	response.Schema = schema.Schema{
		MarkdownDescription: "Experimental native installation of an owned bot app in a standalone or Enterprise workspace. " +
			"Requires a Slack CLI service token scoped to that exact workspace. Uses the CLI's developer APIs, " +
			"whose compatibility and account access must be verified before deployment. " +
			"Create requires confirmed absence; import an existing installation instead. " +
			"Scope/domain changes reinstall; deleting this resource uninstalls the app, without deleting its definition. " +
			"Organization-wide installation and third-party installation are unsupported. " +
			"Use lifecycle.prevent_destroy where uninstall requires review.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"app_id": schema.StringAttribute{
				Required:      true,
				Validators:    []validator.String{stringvalidator.RegexMatches(installationResourceAppID, "App IDs must start with A and contain uppercase letters or digits.")},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"workspace_id": schema.StringAttribute{
				Required:      true,
				Validators:    []validator.String{stringvalidator.RegexMatches(installationResourceWorkspaceID, "Workspace IDs must start with T and contain uppercase letters or digits.")},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"bot_scopes": schema.SetAttribute{
				MarkdownDescription: "Requested bot scopes for install/reinstall. These are inputs, not an attestation of existing token grants.",
				Required:            true, ElementType: types.StringType,
				Validators: []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1))},
			},
			"outgoing_domains": schema.SetAttribute{
				MarkdownDescription: "Outgoing domains passed to install/reinstall.",
				Optional:            true, Computed: true, ElementType: types.StringType,
				Default: setdefault.StaticValue(types.SetValueMust(types.StringType, nil)),
			},
			"installed": schema.BoolAttribute{
				MarkdownDescription: "True after apps.status confirms installation. Null when an attempted mutation cannot be confirmed.",
				Computed:            true,
			},
			"bot_token": schema.StringAttribute{
				MarkdownDescription: "Bot token returned by install/reinstall. Sensitive state; not recoverable on read/import.",
				Computed:            true, Sensitive: true,
			},
			"app_level_token": schema.StringAttribute{
				MarkdownDescription: "App-level token returned by install/reinstall. Sensitive state; not recoverable on read/import.",
				Computed:            true, Sensitive: true,
			},
			"user_token": schema.StringAttribute{
				MarkdownDescription: "User token returned by install/reinstall. Sensitive state; not recoverable on read/import.",
				Computed:            true, Sensitive: true,
			},
		},
	}
}

func (r *installationResource) Configure(_ context.Context, request resource.ConfigureRequest, response *resource.ConfigureResponse) {
	if request.ProviderData == nil {
		return
	}
	client, ok := request.ProviderData.(*slack.Client)
	if !ok {
		response.Diagnostics.AddError("Invalid provider configuration", "Expected a Slack client.")
		return
	}
	r.client = client
}

func (r *installationResource) Create(ctx context.Context, request resource.CreateRequest, response *resource.CreateResponse) {
	var data installationResourceModel
	response.Diagnostics.Append(request.Plan.Get(ctx, &data)...)
	if response.Diagnostics.HasError() {
		return
	}
	status, err := r.observe(ctx, data)
	if err != nil || status != slack.InstallationUninstalled {
		detail := "Create requires confirmed absence. Import an existing installation; an unknown observation cannot authorize installation."
		if err != nil {
			detail += " " + err.Error()
		}
		response.Diagnostics.AddError("Installation precondition not established", detail)
		return
	}
	data.ID = types.StringValue(data.AppID.ValueString() + "/" + data.WorkspaceID.ValueString())
	data.Installed = types.BoolNull()
	data.BotToken, data.AppLevelToken, data.UserToken = types.StringNull(), types.StringNull(), types.StringNull()
	mayHaveInstalled, err := r.install(ctx, &data)
	if err != nil && !mayHaveInstalled {
		response.Diagnostics.AddError("Installation was rejected", err.Error())
		return
	}
	// WHY: A Create error taints saved state and schedules replacement. Save an
	// uncertain identity with a warning; ModifyPlan blocks changes until Read
	// confirms installation or removes a confirmed absent object.
	response.Diagnostics.Append(response.State.Set(ctx, &data)...)
	if err != nil {
		response.Diagnostics.AddWarning("Installation attempt was not confirmed", err.Error()+" The app/workspace identity was retained without taint. Refresh to reconcile; installed remains null and changes are blocked until confirmed.")
	}
}

func (r *installationResource) ModifyPlan(ctx context.Context, request resource.ModifyPlanRequest, response *resource.ModifyPlanResponse) {
	if request.State.Raw.IsNull() {
		return
	}
	var data installationResourceModel
	response.Diagnostics.Append(request.State.Get(ctx, &data)...)
	if !response.Diagnostics.HasError() && data.Installed.IsNull() {
		response.Diagnostics.AddError("Installation requires reconciliation", "The prior installation attempt is unconfirmed. Refresh with a valid workspace token before changing or destroying it; do not skip refresh.")
	}
}

func (r *installationResource) Read(ctx context.Context, request resource.ReadRequest, response *resource.ReadResponse) {
	var data installationResourceModel
	response.Diagnostics.Append(request.State.Get(ctx, &data)...)
	if response.Diagnostics.HasError() {
		return
	}
	status, err := r.observe(ctx, data)
	if err != nil {
		response.Diagnostics.AddError("Unable to observe installation", err.Error())
		return
	}
	switch status {
	case slack.InstallationUninstalled:
		response.State.RemoveResource(ctx)
	case slack.InstallationInstalled:
		data.Installed = types.BoolValue(true)
		response.Diagnostics.Append(response.State.Set(ctx, &data)...)
	default:
		response.Diagnostics.AddError("Installation status is unknown", "Slack omitted the app or its installation flag. State was preserved; check the token's visibility.")
	}
}

func (r *installationResource) Update(ctx context.Context, request resource.UpdateRequest, response *resource.UpdateResponse) {
	var before, after installationResourceModel
	response.Diagnostics.Append(request.State.Get(ctx, &before)...)
	response.Diagnostics.Append(request.Plan.Get(ctx, &after)...)
	if response.Diagnostics.HasError() {
		return
	}
	status, err := r.observe(ctx, before)
	if err != nil || status != slack.InstallationInstalled {
		detail := "Reinstall requires a confirmed existing installation."
		if err != nil {
			detail += " " + err.Error()
		}
		response.Diagnostics.AddError("Reinstall precondition not established", detail)
		return
	}
	after.BotToken, after.AppLevelToken, after.UserToken = before.BotToken, before.AppLevelToken, before.UserToken
	after.Installed = types.BoolNull()
	_, err = r.install(ctx, &after)
	if err != nil {
		// WHY: A failed reinstall must not record unconfirmed requested scopes as
		// applied. Returned tokens can still replace prior tokens after an attempt.
		before.BotToken, before.AppLevelToken, before.UserToken = after.BotToken, after.AppLevelToken, after.UserToken
		before.Installed = after.Installed
		response.Diagnostics.Append(response.State.Set(ctx, &before)...)
		response.Diagnostics.AddError("Reinstall attempt was not confirmed", err.Error()+" Prior requested configuration was preserved.")
		return
	}
	response.Diagnostics.Append(response.State.Set(ctx, &after)...)
}

func (r *installationResource) Delete(ctx context.Context, request resource.DeleteRequest, response *resource.DeleteResponse) {
	var data installationResourceModel
	response.Diagnostics.Append(request.State.Get(ctx, &data)...)
	if response.Diagnostics.HasError() {
		return
	}
	status, err := r.observe(ctx, data)
	if err != nil || status == slack.InstallationUnknown {
		detail := "Uninstall requires an observation of the target installation."
		if err != nil {
			detail += " " + err.Error()
		}
		response.Diagnostics.AddError("Uninstall precondition not established", detail)
		return
	}
	if status == slack.InstallationUninstalled {
		return
	}
	if err := r.client.DeveloperAppUninstall(ctx, data.WorkspaceID.ValueString(), data.AppID.ValueString()); err != nil {
		response.Diagnostics.AddError("Uninstall attempt was not confirmed", err.Error())
		return
	}
	status, err = r.observe(ctx, data)
	if err != nil || status != slack.InstallationUninstalled {
		detail := "Slack did not confirm removal. State was preserved for reconciliation."
		if err != nil {
			detail += " " + err.Error()
		}
		response.Diagnostics.AddError("Uninstall attempt was not confirmed", detail)
	}
}

func (r *installationResource) ImportState(ctx context.Context, request resource.ImportStateRequest, response *resource.ImportStateResponse) {
	parts := strings.Split(request.ID, "/")
	if len(parts) != 2 || !installationResourceAppID.MatchString(parts[0]) || !installationResourceWorkspaceID.MatchString(parts[1]) {
		response.Diagnostics.AddError("Invalid import identity", "Use APP_ID/WORKSPACE_ID, for example A012345/T012345.")
		return
	}
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("id"), request.ID)...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("app_id"), parts[0])...)
	response.Diagnostics.Append(response.State.SetAttribute(ctx, path.Root("workspace_id"), parts[1])...)
}

func (r *installationResource) observe(ctx context.Context, data installationResourceModel) (slack.InstallationStatus, error) {
	statuses, err := r.client.DeveloperAppStatus(ctx, data.WorkspaceID.ValueString(), []string{data.AppID.ValueString()})
	return statuses[data.AppID.ValueString()], err
}

func (r *installationResource) install(ctx context.Context, data *installationResourceModel) (mayHaveInstalled bool, err error) {
	var scopes, domains []string
	if diagnostics := data.BotScopes.ElementsAs(ctx, &scopes, false); diagnostics.HasError() {
		return false, fmt.Errorf("bot scopes must be known before installation")
	}
	if diagnostics := data.OutgoingDomains.ElementsAs(ctx, &domains, false); diagnostics.HasError() {
		return false, fmt.Errorf("outgoing domains must be known before installation")
	}
	credentials, err := r.client.DeveloperAppInstall(ctx, slack.DeveloperInstallRequest{
		AppID: data.AppID.ValueString(), WorkspaceID: data.WorkspaceID.ValueString(),
		BotScopes: scopes, OutgoingDomains: domains,
	})
	if credentials.Bot != "" {
		data.BotToken = types.StringValue(credentials.Bot)
	}
	if credentials.AppLevel != "" {
		data.AppLevelToken = types.StringValue(credentials.AppLevel)
	}
	if credentials.User != "" {
		data.UserToken = types.StringValue(credentials.User)
	}
	if err != nil {
		var rejected *slack.ErrorResponse
		if errors.As(err, &rejected) {
			switch rejected.Error_ {
			case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope", "app_approval_request_pending":
				return false, err
			}
		}
		// WHY: Backend and unknown errors can follow partial execution, even
		// when Slack returns ok:false. Refresh must establish the outcome.
		return true, err
	}
	// WHY: A later status rejection does not undo a successful install. Preserve
	// its identity and returned tokens until refresh can reconcile the result.
	status, err := r.observe(ctx, *data)
	if err != nil {
		return true, err
	}
	if status != slack.InstallationInstalled {
		return true, fmt.Errorf("apps.status did not confirm installation")
	}
	data.Installed = types.BoolValue(true)
	return true, nil
}
