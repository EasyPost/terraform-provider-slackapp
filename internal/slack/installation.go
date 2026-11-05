//declscope:core // the experimental installation API used by the provider

package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/ymm-oss/terraform-provider-slackapp/internal/slackapi"
)

// InstallationStatus preserves a missing observation rather than calling it absent.
type InstallationStatus string

const (
	InstallationInstalled   InstallationStatus = "installed"
	InstallationUninstalled InstallationStatus = "uninstalled"
	InstallationUnknown     InstallationStatus = "unknown"
)

type DeveloperInstallRequest struct {
	AppID           string
	BotScopes       []string
	OutgoingDomains []string
	WorkspaceID     string
}

type InstallationTokens struct {
	Bot      string
	AppLevel string
	User     string
}

type installationSecurity string

func (s installationSecurity) DeveloperToken(context.Context, slackapi.OperationName) (slackapi.DeveloperToken, error) {
	return slackapi.DeveloperToken{Token: string(s)}, nil
}

type installationHTTP struct {
	client *http.Client
}

func (h installationHTTP) Do(request *http.Request) (*http.Response, error) {
	request.Header.Set("User-Agent", "easypost/terraform-provider-slackapp")
	tflog.Debug(request.Context(), "Calling a Slack API method", map[string]any{"method": path.Base(request.URL.Path)})
	response, err := h.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	response.Body = &installationBody{
		ReadCloser: response.Body,
		reader:     &io.LimitedReader{R: response.Body, N: 1024*1024 + 1},
	}
	return response, nil
}

type installationBody struct {
	io.ReadCloser
	reader *io.LimitedReader
}

func (b *installationBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if b.reader.N == 0 {
		return n, errors.New("slack response exceeded the size limit")
	}
	return n, err
}

func (c *Client) developerClient() (*slackapi.Client, error) {
	if c.developerToken == "" {
		return nil, errors.New("developer_token or SLACK_DEVELOPER_TOKEN is required for installation APIs")
	}
	httpClient := *c.httpClient
	// WHY: Following a 307/308 can repeat a mutation before its outcome is reconciled.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client, err := slackapi.NewClient(c.baseURL, installationSecurity(c.developerToken),
		slackapi.WithClient(installationHTTP{client: &httpClient}))
	if err != nil {
		return nil, errors.New("installation API client could not be configured")
	}
	return client, nil
}

var developerErrorCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,99}$`)

func installationResult(method string, ok bool, code slackapi.OptString, err error) error {
	if err != nil {
		// WARN: ogen decode errors contain response bodies, which may include credentials.
		return fmt.Errorf("%s request or response failed; its outcome is unknown", method)
	}
	if ok {
		return nil
	}
	safeCode := code.Or("")
	if !developerErrorCode.MatchString(safeCode) {
		safeCode = "unrecognized_error"
	}
	return fmt.Errorf("%s was rejected by Slack: %w", method, &ErrorResponse{Error_: safeCode})
}

// validateDeveloperWorkspace prevents a workspace install from silently using
// the service token's different workspace or organization.
func (c *Client) validateDeveloperWorkspace(ctx context.Context, api *slackapi.Client, workspaceID string) error {
	auth, err := api.AuthTest(ctx, &slackapi.EmptyRequest{})
	if err != nil {
		return installationResult("auth.test", false, slackapi.OptString{}, err)
	}
	if err := installationResult("auth.test", auth.Ok, auth.Error, nil); err != nil {
		return err
	}
	if auth.TeamID.Or("") != workspaceID || auth.UserID.Or("") == "" || auth.BotID.Or("") != "" || auth.IsEnterpriseInstall.Or(false) {
		return errors.New("the developer token must belong to a user in the exact target workspace; organization tokens are unsupported")
	}
	return nil
}

// DeveloperAppStatus observes only the supplied IDs, matching the Slack CLI's
// apps.status protocol. It does not discover apps or infer absence from omissions.
func (c *Client) DeveloperAppStatus(ctx context.Context, workspaceID string, appIDs []string) (map[string]InstallationStatus, error) {
	api, err := c.developerClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.validateDeveloperWorkspace(ctx, api, workspaceID); err != nil {
		return nil, err
	}
	response, err := api.AppsStatus(ctx, &slackapi.StatusRequest{AppIds: appIDs, TeamID: workspaceID})
	if err != nil {
		return nil, installationResult("apps.status", false, slackapi.OptString{}, err)
	}
	if err := installationResult("apps.status", response.Ok, response.Error, nil); err != nil {
		return nil, err
	}
	team, teamPresent := response.Team.Get()
	apps, appsPresent := response.Apps.Get()
	if !teamPresent || team.TeamID.Or("") != workspaceID || !appsPresent {
		return nil, errors.New("apps.status did not return a valid observation for the target workspace")
	}
	statuses := make(map[string]InstallationStatus, len(appIDs))
	for _, id := range appIDs {
		statuses[id] = InstallationUnknown
	}
	seen := make(map[string]bool, len(appIDs))
	for _, app := range apps {
		if _, requested := statuses[app.AppID]; !requested || seen[app.AppID] {
			return nil, errors.New("apps.status returned an unexpected or duplicate app ID")
		}
		seen[app.AppID] = true
		installed, observed := app.IsInstalled.Get()
		if !observed {
			continue
		}
		statuses[app.AppID] = InstallationUninstalled
		if installed {
			statuses[app.AppID] = InstallationInstalled
		}
	}
	return statuses, nil
}

// DeveloperAppInstall installs an owned app in the service token's workspace.
// The JSON deliberately has no team_id: the CLI only sends that field for
// organization grants, which this implementation does not support.
func (c *Client) DeveloperAppInstall(ctx context.Context, request DeveloperInstallRequest) (InstallationTokens, error) {
	api, err := c.developerClient()
	if err != nil {
		return InstallationTokens{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.validateDeveloperWorkspace(ctx, api, request.WorkspaceID); err != nil {
		return InstallationTokens{}, err
	}
	response, err := api.AppsDeveloperInstall(ctx, &slackapi.InstallRequest{
		AppID: request.AppID, BotScopes: request.BotScopes, OutgoingDomains: request.OutgoingDomains,
	})
	if err != nil {
		return InstallationTokens{}, installationResult("apps.developerInstall", false, slackapi.OptString{}, err)
	}
	if err := installationResult("apps.developerInstall", response.Ok, response.Error, nil); err != nil {
		return InstallationTokens{}, err
	}
	if response.AppID.Or("") != request.AppID {
		return InstallationTokens{}, errors.New("apps.developerInstall did not confirm the requested app ID")
	}
	credentials, _ := response.APIAccessTokens.Get()
	return InstallationTokens{
		Bot: credentials.Bot.Or(""), AppLevel: credentials.AppLevel.Or(""), User: credentials.User.Or(""),
	}, nil
}

func (c *Client) DeveloperAppUninstall(ctx context.Context, workspaceID, appID string) error {
	api, err := c.developerClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.validateDeveloperWorkspace(ctx, api, workspaceID); err != nil {
		return err
	}
	response, err := api.AppsDeveloperUninstall(ctx, &slackapi.UninstallRequest{AppID: appID, TeamID: workspaceID})
	if err != nil {
		return installationResult("apps.developerUninstall", false, slackapi.OptString{}, err)
	}
	return installationResult("apps.developerUninstall", response.Ok, response.Error, nil)
}
