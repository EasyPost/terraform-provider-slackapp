//declscope:core // the experimental installation API used by the provider

package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/ymm-oss/terraform-provider-slackapp/internal/slack/tokens"
)

// InstallationStatus preserves a missing observation rather than calling it absent.
type InstallationStatus string

const (
	InstallationInstalled   InstallationStatus = "installed"
	InstallationUninstalled InstallationStatus = "uninstalled"
	InstallationUnknown     InstallationStatus = "unknown"
)

type DeveloperInstallRequest struct {
	AppID           string   `json:"app_id"`
	BotScopes       []string `json:"bot_scopes"`
	OutgoingDomains []string `json:"outgoing_domains,omitempty"`
	WorkspaceID     string   `json:"-"`
}

type InstallationTokens struct {
	Bot      string `json:"bot"`
	AppLevel string `json:"app_level"`
	User     string `json:"user"`
}

type developerResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

var developerErrorCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,99}$`)

// callDeveloper uses the service token directly; it never rotates configuration
// tokens or retries a mutation whose outcome might be unknown.
func (c *Client) callDeveloper(ctx context.Context, method string, input, output any) error {
	if c.developerToken == "" {
		return errors.New("developer_token or SLACK_DEVELOPER_TOKEN is required for installation APIs")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	request, err := c.createJSONRequest(ctx, http.MethodPost, method,
		tokens.AppConfigurationToken(c.developerToken), input)
	if err != nil {
		return fmt.Errorf("%s request could not be built", method)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s request failed; its outcome is unknown: %w", method, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d; its outcome is unconfirmed", method, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return fmt.Errorf("%s response could not be read; its outcome is unknown", method)
	}
	var envelope developerResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("%s returned invalid JSON; its outcome is unknown", method)
	}
	if !envelope.OK {
		code := envelope.Error
		if !developerErrorCode.MatchString(code) {
			code = "unrecognized_error"
		}
		return fmt.Errorf("%s was rejected by Slack: %w", method, &ErrorResponse{Error_: code})
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("%s returned an invalid response; its outcome is unknown", method)
	}
	return nil
}

// validateDeveloperWorkspace prevents a workspace install from silently using
// the service token's different workspace or organization.
func (c *Client) validateDeveloperWorkspace(ctx context.Context, workspaceID string) error {
	var auth struct {
		TeamID              string `json:"team_id"`
		UserID              string `json:"user_id"`
		BotID               string `json:"bot_id"`
		IsEnterpriseInstall bool   `json:"is_enterprise_install"`
	}
	if err := c.callDeveloper(ctx, "auth.test", struct{}{}, &auth); err != nil {
		return err
	}
	if auth.TeamID != workspaceID || auth.UserID == "" || auth.BotID != "" || auth.IsEnterpriseInstall {
		return errors.New("the developer token must belong to a user in the exact target workspace; organization tokens are unsupported")
	}
	return nil
}

// DeveloperAppStatus observes only the supplied IDs, matching the Slack CLI's
// apps.status protocol. It does not discover apps or infer absence from omissions.
func (c *Client) DeveloperAppStatus(ctx context.Context, workspaceID string, appIDs []string) (map[string]InstallationStatus, error) {
	if err := c.validateDeveloperWorkspace(ctx, workspaceID); err != nil {
		return nil, err
	}
	request := struct {
		AppIDs []string `json:"app_ids"`
		TeamID string   `json:"team_id"`
	}{appIDs, workspaceID}
	var response struct {
		Apps *[]struct {
			AppID     string `json:"app_id"`
			Installed *bool  `json:"is_installed"`
		} `json:"apps"`
		Team struct {
			TeamID string `json:"team_id"`
		} `json:"team"`
	}
	if err := c.callDeveloper(ctx, "apps.status", request, &response); err != nil {
		return nil, err
	}
	if response.Team.TeamID != workspaceID || response.Apps == nil {
		return nil, errors.New("apps.status did not return a valid observation for the target workspace")
	}
	statuses := make(map[string]InstallationStatus, len(appIDs))
	for _, id := range appIDs {
		statuses[id] = InstallationUnknown
	}
	seen := make(map[string]bool, len(appIDs))
	for _, app := range *response.Apps {
		if _, requested := statuses[app.AppID]; !requested || seen[app.AppID] {
			return nil, errors.New("apps.status returned an unexpected or duplicate app ID")
		}
		seen[app.AppID] = true
		if app.Installed == nil {
			continue
		}
		statuses[app.AppID] = InstallationUninstalled
		if *app.Installed {
			statuses[app.AppID] = InstallationInstalled
		}
	}
	return statuses, nil
}

// DeveloperAppInstall installs an owned app in the service token's workspace.
// The JSON deliberately has no team_id: the CLI only sends that field for
// organization grants, which this implementation does not support.
func (c *Client) DeveloperAppInstall(ctx context.Context, request DeveloperInstallRequest) (InstallationTokens, error) {
	if err := c.validateDeveloperWorkspace(ctx, request.WorkspaceID); err != nil {
		return InstallationTokens{}, err
	}
	var response struct {
		AppID  string             `json:"app_id"`
		Tokens InstallationTokens `json:"api_access_tokens"`
	}
	if err := c.callDeveloper(ctx, "apps.developerInstall", request, &response); err != nil {
		return InstallationTokens{}, err
	}
	if response.AppID != request.AppID {
		return InstallationTokens{}, errors.New("apps.developerInstall did not confirm the requested app ID")
	}
	return response.Tokens, nil
}

func (c *Client) DeveloperAppUninstall(ctx context.Context, workspaceID, appID string) error {
	if err := c.validateDeveloperWorkspace(ctx, workspaceID); err != nil {
		return err
	}
	request := struct {
		AppID  string `json:"app_id"`
		TeamID string `json:"team_id"`
	}{appID, workspaceID}
	return c.callDeveloper(ctx, "apps.developerUninstall", request, &struct{}{})
}
