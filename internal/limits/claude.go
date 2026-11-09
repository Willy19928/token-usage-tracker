package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code's own OAuth settings; the token is refreshed exactly the way
// Claude Code refreshes it.
const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	claudeTokenURL   = "https://platform.claude.com/v1/oauth/token"
	claudeClientID   = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthBeta  = "oauth-2025-04-20"
	// Refresh this long before expiry so a poll never uses a dying token.
	claudeRefreshMargin = 10 * time.Minute
)

// ErrClaudeSignedOut means Claude Code has no saved login on this computer.
var ErrClaudeSignedOut = errors.New("claude code is not signed in")

var errClaudeUnauthorized = errors.New("claude usage: HTTP 401")

// ClaudeCollector polls the official usage endpoint with the login Claude
// Code saved, refreshing that login when it is about to expire.
type ClaudeCollector struct {
	Store     *Store
	ConfigDir string
	Client    *http.Client
	Now       func() time.Time
	UsageURL  string
	TokenURL  string
	Profile   string

	profileFor string
	account    Account
}

type claudeLimit struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// claudeLimits are the windows the usage endpoint reports by name.
var claudeLimits = []struct {
	key     string
	minutes int
}{
	{"five_hour", 300}, {"seven_day", 10080}, {"seven_day_opus", 10080},
	{"seven_day_sonnet", 10080}, {"seven_day_oauth_apps", 10080}, {"seven_day_cowork", 10080},
}

// ClaudeConfigDir is where Claude Code keeps its settings and login.
func ClaudeConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func (c *ClaudeCollector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *ClaudeCollector) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (c *ClaudeCollector) credentialsPath() string {
	return filepath.Join(firstNonEmpty(c.ConfigDir, ClaudeConfigDir()), ".credentials.json")
}

// Collect takes one reading of every reported window.
func (c *ClaudeCollector) Collect(ctx context.Context) error {
	token, err := c.accessToken(ctx, false)
	if err != nil {
		return err
	}
	if c.profileFor != token {
		if account, err := c.fetchProfile(ctx, token); err == nil {
			c.account, c.profileFor = account, token
		}
	}
	var body map[string]json.RawMessage
	err = c.get(ctx, firstNonEmpty(c.UsageURL, claudeUsageURL), token, &body)
	if errors.Is(err, errClaudeUnauthorized) {
		// The saved expiry can be wrong, for example after another program
		// refreshed the login: refresh once and retry.
		if token, err = c.accessToken(ctx, true); err == nil {
			err = c.get(ctx, firstNonEmpty(c.UsageURL, claudeUsageURL), token, &body)
		}
	}
	if err != nil {
		return err
	}
	now := c.now().UTC()
	if c.account.ID != "" {
		if err := c.Store.RecordAccount(ctx, Claude, "", c.account, now); err != nil {
			return err
		}
	}
	observations := claudeRows(body)
	if observations == nil {
		observations = claudeNamedLimits(body)
	}
	for _, o := range observations {
		o.Provider, o.Account, o.Plan, o.ObservedAt = Claude, c.account.ID, c.account.Plan, now
		if err := c.Store.Observe(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// claudeRow is one meter of the usage endpoint's limits[]: the server decides
// which meters apply, so plan-specific ones (such as a model's own weekly
// limit on Max plans) appear without client changes.
type claudeRow struct {
	Kind     string          `json:"kind"`
	Group    string          `json:"group"`
	Percent  *float64        `json:"percent"`
	ResetsAt *string         `json:"resets_at"`
	Scope    json.RawMessage `json:"scope"`
}

// claudeRows reads limits[]; nil when the reply has no such list.
func claudeRows(body map[string]json.RawMessage) []Observation {
	raw, ok := body["limits"]
	var rows []claudeRow
	if !ok || json.Unmarshal(raw, &rows) != nil || rows == nil {
		return nil
	}
	out := []Observation{}
	for _, row := range rows {
		if row.Percent == nil || row.ResetsAt == nil || row.Kind == "" {
			continue
		}
		resets, err := time.Parse(time.RFC3339Nano, *row.ResetsAt)
		if err != nil {
			continue
		}
		minutes := map[string]int{"session": 300, "weekly": 10080}[row.Group]
		// The two plan-wide meters keep the names of the endpoint's fields.
		key, label := row.Kind, row.Kind
		switch row.Kind {
		case "session":
			key, label = "five_hour", "five_hour"
		case "weekly_all":
			key, label = "seven_day", "seven_day"
		default:
			if id, name := claudeScope(row.Scope); id != "" {
				key, label = row.Kind+":"+id, firstNonEmpty(name, id)
			}
		}
		out = append(out, Observation{Key: key, Label: label, WindowMinutes: minutes, UsedPercent: *row.Percent, ResetsAt: resets})
	}
	return out
}

// claudeScope names what a scoped meter is for, a model or a surface.
func claudeScope(raw json.RawMessage) (id, label string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, text
	}
	var scope map[string]any
	if json.Unmarshal(raw, &scope) != nil {
		return "", ""
	}
	pick := func(names ...string) string {
		for _, name := range names {
			if value, ok := scope[name].(string); ok && value != "" {
				return value
			}
		}
		return ""
	}
	id = pick("id", "key", "model", "surface", "name", "value")
	label = pick("display_label", "display_name", "label", "name")
	return firstNonEmpty(id, strings.ToLower(label)), label
}

// claudeNamedLimits reads the endpoint's named fields, for replies that
// predate limits[].
func claudeNamedLimits(body map[string]json.RawMessage) []Observation {
	var out []Observation
	for _, limit := range claudeLimits {
		var value claudeLimit
		if raw, ok := body[limit.key]; !ok || json.Unmarshal(raw, &value) != nil || value.Utilization == nil || value.ResetsAt == nil {
			continue
		}
		resets, err := time.Parse(time.RFC3339Nano, *value.ResetsAt)
		if err != nil {
			continue
		}
		out = append(out, Observation{Key: limit.key, Label: limit.key, WindowMinutes: limit.minutes, UsedPercent: *value.Utilization, ResetsAt: resets})
	}
	return out
}

// accessToken returns a valid access token, refreshing and saving the login
// first when it expires soon. Every field Claude Code keeps is preserved.
func (c *ClaudeCollector) accessToken(ctx context.Context, force bool) (string, error) {
	path := c.credentialsPath()
	file, oauth, err := readClaudeCredentials(path)
	if err != nil {
		return "", err
	}
	access, _ := oauth["accessToken"].(string)
	refresh, _ := oauth["refreshToken"].(string)
	expires, _ := oauth["expiresAt"].(float64)
	if access == "" {
		return "", ErrClaudeSignedOut
	}
	if !force && (expires == 0 || c.now().Add(claudeRefreshMargin).Before(time.UnixMilli(int64(expires)))) {
		return access, nil
	}
	if refresh == "" {
		return "", fmt.Errorf("claude login expired and has no refresh token")
	}
	request := map[string]any{"grant_type": "refresh_token", "refresh_token": refresh, "client_id": claudeClientID}
	if scopes, ok := oauth["scopes"].([]any); ok && len(scopes) > 0 {
		names := make([]string, 0, len(scopes))
		for _, scope := range scopes {
			if name, ok := scope.(string); ok {
				names = append(names, name)
			}
		}
		request["scope"] = strings.Join(names, " ")
	}
	payload, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, firstNonEmpty(c.TokenURL, claudeTokenURL), strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("claude token refresh: HTTP %d", res.StatusCode)
	}
	var refreshed struct {
		AccessToken           string `json:"access_token"`
		RefreshToken          string `json:"refresh_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	if err := json.Unmarshal(data, &refreshed); err != nil || refreshed.AccessToken == "" || refreshed.ExpiresIn <= 0 {
		return "", fmt.Errorf("claude token refresh: unexpected response")
	}
	// Re-read right before writing so a concurrent Claude Code save wins
	// for every field this refresh does not own.
	file, oauth, err = readClaudeCredentials(path)
	if err != nil {
		return "", err
	}
	now := c.now()
	oauth["accessToken"] = refreshed.AccessToken
	oauth["refreshToken"] = firstNonEmpty(refreshed.RefreshToken, refresh)
	oauth["expiresAt"] = now.Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	if refreshed.RefreshTokenExpiresIn > 0 {
		oauth["refreshTokenExpiresAt"] = now.Add(time.Duration(refreshed.RefreshTokenExpiresIn) * time.Second).UnixMilli()
	}
	file["claudeAiOauth"] = oauth
	if err := writeFileAtomic(path, file); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

func readClaudeCredentials(path string) (map[string]any, map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrClaudeSignedOut
	}
	if err != nil {
		return nil, nil, err
	}
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("claude credentials: %w", err)
	}
	oauth, ok := file["claudeAiOauth"].(map[string]any)
	if !ok {
		return nil, nil, ErrClaudeSignedOut
	}
	return file, oauth, nil
}

func writeFileAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (c *ClaudeCollector) get(ctx context.Context, url, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("User-Agent", "token-usage-tracker")
	res, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return errClaudeUnauthorized
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("claude usage: HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

func (c *ClaudeCollector) fetchProfile(ctx context.Context, token string) (Account, error) {
	var profile struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
			Max   bool   `json:"has_claude_max"`
			Pro   bool   `json:"has_claude_pro"`
		} `json:"account"`
		Organization struct {
			UUID            string `json:"uuid"`
			Type            string `json:"organization_type"`
			RateLimitTier   string `json:"rate_limit_tier"`
			PlanDisplayName string `json:"plan_display_name"`
			SeatTier        any    `json:"seat_tier"`
		} `json:"organization"`
	}
	if err := c.get(ctx, firstNonEmpty(c.Profile, claudeProfileURL), token, &profile); err != nil {
		return Account{}, err
	}
	id := profile.Account.UUID
	if profile.Organization.UUID != "" {
		id += "/" + profile.Organization.UUID
	}
	seat, _ := profile.Organization.SeatTier.(string)
	plan := claudePlanLabel(profile.Organization.PlanDisplayName, profile.Organization.Type,
		profile.Organization.RateLimitTier, seat, profile.Account.Max, profile.Account.Pro)
	return Account{ID: id, Email: profile.Account.Email, Plan: plan}, nil
}

// claudePlanLabel names the plan the way Claude does: the server's own display
// name when it sends one, else the organization type with the Max usage tier
// (5x, 20x) or the Team/Enterprise seat type.
func claudePlanLabel(display, orgType, rateTier, seatTier string, max, pro bool) string {
	if display = strings.TrimSpace(display); display != "" {
		return display
	}
	plan := map[string]string{"claude_max": "Max", "claude_pro": "Pro", "claude_team": "Team", "claude_enterprise": "Enterprise"}[orgType]
	if plan == "" {
		switch {
		case max:
			plan = "Max"
		case pro:
			plan = "Pro"
		default:
			return ""
		}
	}
	switch plan {
	case "Max":
		if strings.Contains(rateTier, "20x") {
			return "Max 20x"
		}
		if strings.Contains(rateTier, "5x") {
			return "Max 5x"
		}
	case "Team", "Enterprise":
		if seat := claudeSeat(seatTier); seat != "" {
			return plan + " · " + seat
		}
	}
	return plan
}

// claudeSeat groups Claude's seat tiers the way Claude Code does.
func claudeSeat(tier string) string {
	switch tier {
	case "team_tier_1", "team_bendep_nonprofit_premium", "team_labs_premium", "enterprise_tier_1", "enterprise_bendep_premium":
		return "Premium"
	case "team_standard", "team_bendep_nonprofit_standard", "team_labs_standard", "enterprise_standard", "enterprise_nonprofit", "enterprise_higher_ed":
		return "Standard"
	case "enterprise_usage_based", "enterprise_usage_based_chat", "enterprise_hipaa_chat", "enterprise_hipaa_chat_and_code":
		return "Usage-based"
	}
	return ""
}
